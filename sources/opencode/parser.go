package opencode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// cursor tracks parse position between calls.
// Format: "<msg_count>,<tool_count>"
type cursor struct {
	msgCount  int
	toolCount int
}

func encodeCursor(c cursor) string {
	return strconv.Itoa(c.msgCount) + "," + strconv.Itoa(c.toolCount)
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q", s)
	}
	mc, err := strconv.Atoi(parts[0])
	if err != nil {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q: %w", s, err)
	}
	tc, err := strconv.Atoi(parts[1])
	if err != nil {
		return cursor{}, fmt.Errorf("opencode: invalid cursor %q: %w", s, err)
	}
	return cursor{msgCount: mc, toolCount: tc}, nil
}

// openReadOnly opens the SQLite database in read-only mode.
// Returns (nil, nil) if the file does not exist.
func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opencode: stat %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
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
func discoverSessions(db *sql.DB, cutoff time.Time) ([]sessionRow, error) {
	rows, err := db.Query(
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
}

// queryParseData queries the database for session parse data and computes deltas.
func queryParseData(db *sql.DB, sessionID string, cur cursor) (parseResult, cursor, error) {
	// Query session row.
	var s sessionRow
	err := db.QueryRow(
		`SELECT id, slug, directory, model, COALESCE(tokens_input, 0), COALESCE(tokens_output, 0),
		 created_at, updated_at, time_archived
		 FROM session WHERE id = ?`, sessionID,
	).Scan(&s.id, &s.slug, &s.directory, &s.modelJSON,
		&s.tokensInput, &s.tokensOutput,
		&s.createdAt, &s.updatedAt, &s.timeArchived)
	if err != nil {
		if err == sql.ErrNoRows {
			return parseResult{}, cur, nil
		}
		return parseResult{}, cur, fmt.Errorf("opencode: query session %s: %w", sessionID, err)
	}

	// Count total messages.
	var msgCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM message WHERE session_id = ?`, sessionID,
	).Scan(&msgCount); err != nil {
		return parseResult{}, cur, fmt.Errorf("opencode: count messages: %w", err)
	}

	// Count total tool-invocation parts.
	var toolCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'`,
		sessionID,
	).Scan(&toolCount); err != nil {
		return parseResult{}, cur, fmt.Errorf("opencode: count tool parts: %w", err)
	}

	// Get latest tool-invocation part's toolName.
	var currentTool string
	var toolData sql.NullString
	err = db.QueryRow(
		`SELECT data FROM part WHERE session_id = ?
		 AND json_extract(data, '$.type') = 'tool-invocation'
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&toolData)
	if err != nil && err != sql.ErrNoRows {
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
	err = db.QueryRow(
		`SELECT data, updated_at FROM message WHERE session_id = ?
		 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&msgData, &msgUpdatedAt)
	if err != nil && err != sql.ErrNoRows {
		return parseResult{}, cur, fmt.Errorf("opencode: query latest message: %w", err)
	}
	if msgData.Valid {
		lastRole = extractMessageRole(msgData.String)
	}
	if msgUpdatedAt.Valid {
		lastActivity = parseTimestamp(msgUpdatedAt.String)
	}
	if lastActivity.IsZero() {
		lastActivity = parseTimestamp(s.updatedAt)
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
	}

	newCur := cursor{
		msgCount:  msgCount,
		toolCount: toolCount,
	}

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
