package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mrf/agentwatch/internal/sqlitero"
)

var errInvalidCursor = errors.New("opencode: invalid cursor")

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
		return cursor{}, fmt.Errorf("%w %q", errInvalidCursor, s)
	}
	n, err := parseCursorFields(s, parts)
	if err != nil {
		return cursor{}, err
	}
	archive := n[5]
	if archive < int64(archiveNone) || archive > int64(archiveConfirmed) {
		return cursor{}, fmt.Errorf("%w %q: bad archive state", errInvalidCursor, s)
	}
	if n[6] != 0 && n[6] != 1 {
		return cursor{}, fmt.Errorf("%w %q: bad working flag", errInvalidCursor, s)
	}
	return cursor{
		msgCount:     int(n[0]),
		toolCount:    int(n[1]),
		tokensInput:  int(n[2]),
		tokensOutput: int(n[3]),
		lastActivity: n[4],
		archive:      archiveState(archive),
		working:      n[6] == 1,
		seen:         true,
	}, nil
}

// parseCursorFields parses the comma-separated integers of cursor s; absent
// trailing fields are zero.
func parseCursorFields(s string, parts []string) ([7]int64, error) {
	var n [7]int64
	for i := 0; i < len(parts); i++ {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return n, fmt.Errorf("%w %q: %w", errInvalidCursor, s, err)
		}
		n[i] = v
	}
	return n, nil
}

// openReadOnly opens the SQLite database in read-only mode.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sqlitero.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opencode: open %s: %w", path, err)
	}
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("opencode: iterate sessions: %w", err)
	}
	return sessions, nil
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
	s, found, err := querySession(ctx, db, sessionID)
	if err != nil || !found {
		return parseResult{}, cur, err
	}

	// An archived session is reported terminal once; after that it is silent
	// until it is unarchived.
	first := !cur.seen
	terminal := s.timeArchived.Valid && s.timeArchived.String != ""
	if terminal && cur.archive == archiveConfirmed {
		return parseResult{}, cur, nil
	}

	act, err := queryActivity(ctx, db, sessionID, s.updatedAt)
	if err != nil {
		return parseResult{}, cur, err
	}
	msgCount, toolCount, state, lastActivity := act.msgCount, act.toolCount, act.state, act.lastActivity

	var archivedAt time.Time
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
		// Clamp deltas to zero for safety.
		msgDelta:    max(msgCount-cur.msgCount, 0),
		toolDelta:   max(toolCount-cur.toolCount, 0),
		currentTool: act.currentTool,
		lastRole:    state.role,
		inProgress:  state.inProgress,
		replyDone:   state.replyDone,
	}

	newCur := nextCursor(&s, &act, terminal, first)
	result.changed = first || newCur != cur

	return result, newCur, nil
}

// nextCursor records what this parse reports, so an unchanged session can
// be recognised next time.
func nextCursor(s *sessionRow, act *sessionActivity, terminal, first bool) cursor {
	c := cursor{
		msgCount:     act.msgCount,
		toolCount:    act.toolCount,
		tokensInput:  s.tokensInput,
		tokensOutput: s.tokensOutput,
		working:      act.state.inProgress,
		seen:         true,
	}
	if !act.lastActivity.IsZero() {
		c.lastActivity = act.lastActivity.UnixNano()
	}
	if terminal {
		c.archive = archiveConfirmed
		if first {
			c.archive = archivePending
		}
	}
	return c
}

// sessionActivity is the per-session data read from the message and part
// tables.
type sessionActivity struct {
	msgCount     int
	toolCount    int
	currentTool  string
	state        messageState
	lastActivity time.Time
}

func queryActivity(ctx context.Context, db *sql.DB, sessionID, sessionUpdatedAt string) (sessionActivity, error) {
	var act sessionActivity
	var err error
	if act.msgCount, act.toolCount, err = queryCounts(ctx, db, sessionID); err != nil {
		return sessionActivity{}, err
	}
	if act.currentTool, err = queryLatestTool(ctx, db, sessionID); err != nil {
		return sessionActivity{}, err
	}
	if act.state, act.lastActivity, err = queryLatestActivity(ctx, db, sessionID, sessionUpdatedAt); err != nil {
		return sessionActivity{}, err
	}
	return act, nil
}

// querySession reads the session row; found is false when it does not exist.
func querySession(ctx context.Context, db *sql.DB, sessionID string) (s sessionRow, found bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT id, slug, directory, model, COALESCE(tokens_input, 0), COALESCE(tokens_output, 0),
		 created_at, updated_at, time_archived
		 FROM session WHERE id = ?`, sessionID,
	).Scan(&s.id, &s.slug, &s.directory, &s.modelJSON,
		&s.tokensInput, &s.tokensOutput,
		&s.createdAt, &s.updatedAt, &s.timeArchived)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRow{}, false, nil
	}
	if err != nil {
		return sessionRow{}, false, fmt.Errorf("opencode: query session %s: %w", sessionID, err)
	}
	return s, true, nil
}

// queryCounts returns the session's total message and tool-invocation counts.
func queryCounts(ctx context.Context, db *sql.DB, sessionID string) (msgCount, toolCount int, err error) {
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message WHERE session_id = ?`, sessionID,
	).Scan(&msgCount); err != nil {
		return 0, 0, fmt.Errorf("opencode: count messages: %w", err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'`,
		sessionID,
	).Scan(&toolCount); err != nil {
		return 0, 0, fmt.Errorf("opencode: count tool parts: %w", err)
	}
	return msgCount, toolCount, nil
}

// queryLatestTool returns the toolName of the newest tool-invocation part, or
// "" if there is none.
func queryLatestTool(ctx context.Context, db *sql.DB, sessionID string) (string, error) {
	var toolData sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT data FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&toolData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("opencode: query latest tool: %w", err)
	}
	if !toolData.Valid {
		return "", nil
	}
	return extractToolName(toolData.String), nil
}

// queryLatestActivity returns the newest message's state and the session's
// last activity time: the newest message or part write, falling back to the
// session row's sessionUpdatedAt.
func queryLatestActivity(ctx context.Context, db *sql.DB, sessionID, sessionUpdatedAt string) (messageState, time.Time, error) {
	var msgData sql.NullString
	var msgUpdatedAt sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT data, updated_at FROM message WHERE session_id = ?
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&msgData, &msgUpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return messageState{}, time.Time{}, fmt.Errorf("opencode: query latest message: %w", err)
	}
	var state messageState
	if msgData.Valid {
		state = extractMessageState(msgData.String)
	}
	var lastActivity time.Time
	if msgUpdatedAt.Valid {
		lastActivity = parseTimestamp(msgUpdatedAt.String)
	}
	if lastActivity.IsZero() {
		lastActivity = parseTimestamp(sessionUpdatedAt)
	}

	// Streaming rewrites parts, not messages, so the newest part write counts
	// as activity too.
	var partUpdatedAt sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(updated_at) FROM part WHERE session_id = ?`, sessionID,
	).Scan(&partUpdatedAt); err != nil {
		return messageState{}, time.Time{}, fmt.Errorf("opencode: query latest part: %w", err)
	}
	if t := parseTimestamp(nullStr(partUpdatedAt)); t.After(lastActivity) {
		lastActivity = t
	}
	return state, lastActivity, nil
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
