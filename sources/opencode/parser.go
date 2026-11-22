package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// archiveState records how far the archived-session terminal report has got.
type archiveState int

const (
	archiveNone archiveState = iota
	// archivePending: Terminal was reported on the session's first sight. The
	// monitor registers a session first seen with Terminal=true as active, so
	// the report is repeated once to land the terminal transition.
	archivePending
	// archiveConfirmed: the terminal transition has been reported; stay silent.
	archiveConfirmed
)

// cursor records what was last reported for a session, so Parse can return a
// zero update when nothing changed.
// Format: "<msgs>,<tools>,<tokensIn>,<tokensOut>,<lastActivityUnixNano>,<archive>,<working>".
// The shorter forms "<msgs>,<tools>" and the six-field form without
// <working> are still accepted.
type cursor struct {
	msgCount     int
	toolCount    int
	tokensInput  int
	tokensOutput int
	lastActivity int64
	archive      archiveState
	// working is true when the last report had an assistant reply in flight,
	// so its completion is reported even if no other field moved.
	working bool
	// seen is true for any decoded (non-empty) cursor; it is implied by the
	// encoding, not stored in it.
	seen bool
}

func encodeCursor(c cursor) string {
	return strconv.Itoa(c.msgCount) + "," + strconv.Itoa(c.toolCount) + "," +
		strconv.Itoa(c.tokensInput) + "," + strconv.Itoa(c.tokensOutput) + "," +
		strconv.FormatInt(c.lastActivity, 10) + "," + strconv.Itoa(int(c.archive)) + "," +
		strconv.Itoa(boolToInt(c.working))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 2 && len(parts) != 6 && len(parts) != 7 {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q", s)
	}
	var n [7]int64
	for i := 0; i < len(parts); i++ {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return cursor{}, fmt.Errorf("opencode: invalid cursor %q: %w", s, err)
		}
		n[i] = v
	}
	if n[5] < int64(archiveNone) || n[5] > int64(archiveConfirmed) {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q: bad archive state", s)
	}
	if n[6] != 0 && n[6] != 1 {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q: bad working flag", s)
	}
	return cursor{
		msgCount:     int(n[0]),
		toolCount:    int(n[1]),
		tokensInput:  int(n[2]),
		tokensOutput: int(n[3]),
		lastActivity: n[4],
		archive:      archiveState(n[5]),
		working:      n[6] == 1,
		seen:         true,
	}, nil
}

// dsn builds a read-only SQLite URI for path. The path is URL-escaped so that
// '?', '#' and '%' in it are not read as URI syntax.
func dsn(path string) string {
	p := filepath.ToSlash(path)
	if len(p) > 1 && p[1] == ':' {
		p = "/" + p // Windows drive letter
	}
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     p,
		RawQuery: url.Values{
			"mode":    {"ro"},
			"_pragma": {"busy_timeout(5000)"},
		}.Encode(),
	}
	return u.String()
}

// openReadOnly opens the SQLite database in read-only mode.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opencode: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// sessionRow holds fields queried from the session table.
type sessionRow struct {
	id           string
	slug         sql.NullString
	directory    sql.NullString
	modelJSON    sql.NullString
	tokensInput  int
	tokensOutput int
	createdAt    string
	updatedAt    string
	timeArchived sql.NullString
}

// discoverSessions queries the session table, optionally filtering by cutoff.
func discoverSessions(ctx context.Context, db *sql.DB, cutoff time.Time) ([]sessionRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, slug, directory, model, COALESCE(tokens_input, 0), COALESCE(tokens_output, 0),
		 created_at, updated_at, time_archived
		 FROM session ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("opencode: query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []sessionRow
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.id, &s.slug, &s.directory, &s.modelJSON,
			&s.tokensInput, &s.tokensOutput,
			&s.createdAt, &s.updatedAt, &s.timeArchived); err != nil {
			return nil, fmt.Errorf("opencode: scan session: %w", err)
		}
		if !cutoff.IsZero() {
			updatedAt := parseTimestamp(s.updatedAt)
			if !updatedAt.IsZero() && updatedAt.Before(cutoff) {
				continue
			}
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// parseResult holds data extracted from the database for a single Parse call.
type parseResult struct {
	slug         string
	directory    string
	model        string
	tokensInput  int
	tokensOutput int
	createdAt    time.Time
	lastActivity time.Time
	archivedAt   time.Time
	terminal     bool
	msgDelta     int
	toolDelta    int
	currentTool  string
	lastRole     string
	// inProgress: the latest message is an assistant reply still being
	// written. replyDone: it carries an explicit stop marker. Neither is set
	// for an assistant message with no marker at all.
	inProgress bool
	replyDone  bool
	// changed is false when there is nothing new to report; the caller then
	// returns a zero update.
	changed bool
}

// queryParseData queries the database for session parse data and computes
// deltas. When nothing changed since cur it returns changed=false and cur.
func queryParseData(ctx context.Context, db *sql.DB, sessionID string, cur cursor) (parseResult, cursor, error) {
	// Query session row.
	var s sessionRow
	err := db.QueryRowContext(ctx,
		`SELECT id, slug, directory, model, COALESCE(tokens_input, 0), COALESCE(tokens_output, 0),
		 created_at, updated_at, time_archived
		 FROM session WHERE id = ?`, sessionID,
	).Scan(&s.id, &s.slug, &s.directory, &s.modelJSON,
		&s.tokensInput, &s.tokensOutput,
		&s.createdAt, &s.updatedAt, &s.timeArchived)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return parseResult{}, cur, nil
		}
		return parseResult{}, cur, fmt.Errorf("opencode: query session %s: %w", sessionID, err)
	}

	// An archived session is reported terminal once; after that it is silent
	// until it is unarchived.
	first := !cur.seen
	if s.timeArchived.Valid && s.timeArchived.String != "" && cur.archive == archiveConfirmed {
		return parseResult{}, cur, nil
	}

	// Count total messages.
	var msgCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message WHERE session_id = ?`, sessionID,
	).Scan(&msgCount); err != nil {
		return parseResult{}, cur, fmt.Errorf("opencode: count messages: %w", err)
	}

	// Count total tool-invocation parts.
	var toolCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'`,
		sessionID,
	).Scan(&toolCount); err != nil {
		return parseResult{}, cur, fmt.Errorf("opencode: count tool parts: %w", err)
	}

	// Get latest tool-invocation part's toolName.
	var currentTool string
	var toolData sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT data FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&toolData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return parseResult{}, cur, fmt.Errorf("opencode: query latest tool: %w", err)
	}
	if toolData.Valid {
		currentTool = extractToolName(toolData.String)
	}

	// Get latest message for role and LastActivityAt.
	var lastActivity time.Time
	var lastRole string
	var msgData sql.NullString
	var msgUpdatedAt sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT data, updated_at FROM message WHERE session_id = ?
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&msgData, &msgUpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return parseResult{}, cur, fmt.Errorf("opencode: query latest message: %w", err)
	}
	var state messageState
	if msgData.Valid {
		state = extractMessageState(msgData.String)
		lastRole = state.role
	}
	if msgUpdatedAt.Valid {
		lastActivity = parseTimestamp(msgUpdatedAt.String)
	}
	if lastActivity.IsZero() {
		lastActivity = parseTimestamp(s.updatedAt)
	}

	// Streaming rewrites parts, not messages, so the newest part write counts
	// as activity too.
	var partUpdatedAt sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(updated_at) FROM part WHERE session_id = ?`, sessionID,
	).Scan(&partUpdatedAt); err != nil {
		return parseResult{}, cur, fmt.Errorf("opencode: query latest part: %w", err)
	}
	if t := parseTimestamp(nullStr(partUpdatedAt)); t.After(lastActivity) {
		lastActivity = t
	}

	// Compute deltas (clamp to zero for safety).
	msgDelta := msgCount - cur.msgCount
	if msgDelta < 0 {
		msgDelta = 0
	}
	toolDelta := toolCount - cur.toolCount
	if toolDelta < 0 {
		toolDelta = 0
	}

	var archivedAt time.Time
	terminal := s.timeArchived.Valid && s.timeArchived.String != ""
	if terminal {
		archivedAt = parseTimestamp(s.timeArchived.String)
	}

	result := parseResult{
		slug:         nullStr(s.slug),
		directory:    nullStr(s.directory),
		model:        parseModelJSON(nullStr(s.modelJSON)),
		tokensInput:  s.tokensInput,
		tokensOutput: s.tokensOutput,
		createdAt:    parseTimestamp(s.createdAt),
		lastActivity: lastActivity,
		archivedAt:   archivedAt,
		terminal:     terminal,
		msgDelta:     msgDelta,
		toolDelta:    toolDelta,
		currentTool:  currentTool,
		lastRole:     lastRole,
		inProgress:   state.inProgress,
		replyDone:    state.replyDone,
	}

	newCur := cursor{
		msgCount:     msgCount,
		toolCount:    toolCount,
		tokensInput:  s.tokensInput,
		tokensOutput: s.tokensOutput,
		working:      state.inProgress,
		seen:         true,
	}
	if !lastActivity.IsZero() {
		newCur.lastActivity = lastActivity.UnixNano()
	}
	if terminal {
		newCur.archive = archiveConfirmed
		if first {
			newCur.archive = archivePending
		}
	}
	result.changed = first || newCur != cur

	return result, newCur, nil
}

// parseModelJSON extracts the model ID from a JSON model column value.
// Handles both object ({"id":"model-name",...}) and plain string formats.
func parseModelJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(raw), &obj) == nil && obj.ID != "" {
		return obj.ID
	}
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		return s
	}
	return raw
}

// extractToolName extracts toolName from a tool-invocation part JSON.
func extractToolName(data string) string {
	var part struct {
		ToolName string `json:"toolName"`
	}
	if json.Unmarshal([]byte(data), &part) == nil {
		return part.ToolName
	}
	return ""
}

// extractMessageRole extracts the role from a message data JSON.
func extractMessageRole(data string) string {
	var msg struct {
		Role string `json:"role"`
	}
	if json.Unmarshal([]byte(data), &msg) == nil {
		return msg.Role
	}
	return ""
}

// messageState is what a message's data JSON says about turn progress.
type messageState struct {
	role       string
	inProgress bool
	replyDone  bool
}

// extractMessageState reads OpenCode's assistant-message markers: finish
// "tool-calls" means the step ended but the turn continues; any other finish,
// or time.completed, ends the reply; time.created without either means the
// reply is still streaming. A message with no markers is left undecided.
func extractMessageState(data string) messageState {
	var msg struct {
		Role   string `json:"role"`
		Finish string `json:"finish"`
		Time   struct {
			Created   float64 `json:"created"`
			Completed float64 `json:"completed"`
		} `json:"time"`
	}
	if json.Unmarshal([]byte(data), &msg) != nil {
		return messageState{}
	}
	st := messageState{role: msg.Role}
	if msg.Role != "assistant" {
		return st
	}
	switch {
	case msg.Finish == "tool-calls":
		st.inProgress = true
	case msg.Finish != "" || msg.Time.Completed > 0:
		st.replyDone = true
	case msg.Time.Created > 0:
		st.inProgress = true
	}
	return st
}

// parseTimestamp parses a timestamp string from the SQLite database.
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}
