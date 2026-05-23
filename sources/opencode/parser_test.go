package opencode

import (
	"database/sql"
	"testing"
	"time"
)

// ---- cursor -----------------------------------------------------------------

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{msgCount: 42, toolCount: 7}
	encoded := encodeCursor(c)
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decodeCursor(%q): %v", encoded, err)
	}
	if decoded != c {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, c)
	}
}

func TestDecodeCursorEmpty(t *testing.T) {
	c, err := decodeCursor("")
	if err != nil {
		t.Fatalf("decodeCursor empty: %v", err)
	}
	if c != (cursor{}) {
		t.Errorf("expected zero cursor, got %+v", c)
	}
}

func TestDecodeCursorInvalid(t *testing.T) {
	cases := []string{"abc", "1", "1,2,3", "x,1", "1,x"}
	for _, tc := range cases {
		_, err := decodeCursor(tc)
		if err == nil {
			t.Errorf("decodeCursor(%q) expected error, got nil", tc)
		}
	}
}

// ---- parseModelJSON ---------------------------------------------------------

func TestParseModelJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"object with id", `{"id":"claude-sonnet-4-20250514","providerID":"anthropic","variant":""}`, "claude-sonnet-4-20250514"},
		{"plain string", `"gpt-4o"`, "gpt-4o"},
		{"bare string", `o3-mini`, "o3-mini"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModelJSON(tt.raw)
			if got != tt.want {
				t.Errorf("parseModelJSON(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// ---- extractToolName --------------------------------------------------------

func TestExtractToolName(t *testing.T) {
	got := extractToolName(`{"type":"tool-invocation","toolName":"Bash","args":{},"result":"ok","state":"completed"}`)
	if got != "Bash" {
		t.Errorf("extractToolName = %q, want %q", got, "Bash")
	}
}

func TestExtractToolNameEmpty(t *testing.T) {
	got := extractToolName(`{"type":"text","text":"hello"}`)
	if got != "" {
		t.Errorf("extractToolName = %q, want empty", got)
	}
}

// ---- extractMessageRole -----------------------------------------------------

func TestExtractMessageRole(t *testing.T) {
	got := extractMessageRole(`{"role":"user","parts":[]}`)
	if got != "user" {
		t.Errorf("extractMessageRole = %q, want %q", got, "user")
	}
	got = extractMessageRole(`{"role":"assistant","parts":[]}`)
	if got != "assistant" {
		t.Errorf("extractMessageRole = %q, want %q", got, "assistant")
	}
}

// ---- parseTimestamp ---------------------------------------------------------

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		input string
		want  string // RFC3339Nano in UTC
	}{
		{"", ""},
		{"2026-01-15T10:30:00Z", "2026-01-15T10:30:00Z"},
		{"2026-01-15T10:30:00.123Z", "2026-01-15T10:30:00.123Z"},
		{"2026-01-15 10:30:00", "2026-01-15T10:30:00Z"},
		{"2026-01-15T10:30:00", "2026-01-15T10:30:00Z"},
	}
	for _, tt := range tests {
		got := parseTimestamp(tt.input)
		if tt.want == "" {
			if !got.IsZero() {
				t.Errorf("parseTimestamp(%q) = %v, want zero", tt.input, got)
			}
			continue
		}
		want, _ := time.Parse(time.RFC3339Nano, tt.want)
		if !got.Equal(want) {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tt.input, got, want)
		}
	}
}

// ---- discoverSessions -------------------------------------------------------

func TestDiscoverSessionsAll(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-1", "my-slug", "/home/user/project",
		`{"id":"claude-sonnet-4-20250514"}`, 1000, 200,
		"2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z", "")

	sessions, err := discoverSessions(db, time.Time{})
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].id != "sess-1" {
		t.Errorf("id = %q, want %q", sessions[0].id, "sess-1")
	}
}

func TestDiscoverSessionsWithCutoff(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "old", "old-slug", "/old",
		`{"id":"model"}`, 0, 0,
		"2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z", "")
	seedSession(t, db, "new", "new-slug", "/new",
		`{"id":"model"}`, 0, 0,
		"2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z", "")

	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sessions, err := discoverSessions(db, cutoff)
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session after cutoff, got %d", len(sessions))
	}
	if sessions[0].id != "new" {
		t.Errorf("id = %q, want %q", sessions[0].id, "new")
	}
}

// ---- queryParseData ---------------------------------------------------------

func TestQueryParseDataBasic(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-1", "build-it", "/home/user/project",
		`{"id":"claude-sonnet-4-20250514","providerID":"anthropic"}`, 5000, 200,
		"2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z", "")

	seedMessage(t, db, "msg-1", "sess-1", `{"role":"user","parts":[]}`,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	seedMessage(t, db, "msg-2", "sess-1", `{"role":"assistant","parts":[]}`,
		"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")

	seedPart(t, db, "part-1", "msg-2", "sess-1",
		`{"type":"tool-invocation","toolName":"Bash","args":{},"result":"ok","state":"completed"}`,
		"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
	seedPart(t, db, "part-2", "msg-2", "sess-1",
		`{"type":"text","text":"Done!"}`,
		"2026-01-15T10:01:01Z", "2026-01-15T10:01:01Z")
	seedPart(t, db, "part-3", "msg-2", "sess-1",
		`{"type":"tool-invocation","toolName":"Read","args":{},"result":"content","state":"completed"}`,
		"2026-01-15T10:01:02Z", "2026-01-15T10:01:02Z")

	result, newCur, err := queryParseData(db, "sess-1", cursor{})
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}

	if result.slug != "build-it" {
		t.Errorf("slug = %q, want %q", result.slug, "build-it")
	}
	if result.directory != "/home/user/project" {
		t.Errorf("directory = %q, want %q", result.directory, "/home/user/project")
	}
	if result.model != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q, want %q", result.model, "claude-sonnet-4-20250514")
	}
	if result.tokensInput != 5000 {
		t.Errorf("tokensInput = %d, want %d", result.tokensInput, 5000)
	}
	if result.tokensOutput != 200 {
		t.Errorf("tokensOutput = %d, want %d", result.tokensOutput, 200)
	}
	if result.msgDelta != 2 {
		t.Errorf("msgDelta = %d, want %d", result.msgDelta, 2)
	}
	if result.toolDelta != 2 {
		t.Errorf("toolDelta = %d, want %d", result.toolDelta, 2)
	}
	if result.currentTool != "Read" {
		t.Errorf("currentTool = %q, want %q", result.currentTool, "Read")
	}
	if result.lastRole != "assistant" {
		t.Errorf("lastRole = %q, want %q", result.lastRole, "assistant")
	}
	if result.terminal {
		t.Error("expected non-terminal session")
	}

	// New cursor should reflect totals.
	if newCur.msgCount != 2 {
		t.Errorf("cursor msgCount = %d, want %d", newCur.msgCount, 2)
	}
	if newCur.toolCount != 2 {
		t.Errorf("cursor toolCount = %d, want %d", newCur.toolCount, 2)
	}
}

func TestQueryParseDataDelta(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-1", "", "/project",
		`{"id":"model"}`, 100, 10,
		"2026-01-15T10:00:00Z", "2026-01-15T10:05:00Z", "")

	seedMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	seedMessage(t, db, "msg-2", "sess-1", `{"role":"assistant"}`,
		"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")

	seedPart(t, db, "part-1", "msg-2", "sess-1",
		`{"type":"tool-invocation","toolName":"Bash"}`,
		"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")

	// Simulate having already seen 1 message and 0 tool calls.
	prev := cursor{msgCount: 1, toolCount: 0}
	result, newCur, err := queryParseData(db, "sess-1", prev)
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}
	if result.msgDelta != 1 {
		t.Errorf("msgDelta = %d, want %d (2 total - 1 prev)", result.msgDelta, 1)
	}
	if result.toolDelta != 1 {
		t.Errorf("toolDelta = %d, want %d", result.toolDelta, 1)
	}
	if newCur.msgCount != 2 {
		t.Errorf("cursor msgCount = %d, want %d", newCur.msgCount, 2)
	}
	if newCur.toolCount != 1 {
		t.Errorf("cursor toolCount = %d, want %d", newCur.toolCount, 1)
	}
}

func TestQueryParseDataNoDelta(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-1", "", "/project",
		`{"id":"model"}`, 100, 10,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")

	seedMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")

	// Cursor already reflects current state.
	prev := cursor{msgCount: 1, toolCount: 0}
	result, _, err := queryParseData(db, "sess-1", prev)
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}
	if result.msgDelta != 0 {
		t.Errorf("msgDelta = %d, want 0", result.msgDelta)
	}
	if result.toolDelta != 0 {
		t.Errorf("toolDelta = %d, want 0", result.toolDelta)
	}
}

func TestQueryParseDataNoSession(t *testing.T) {
	db := createTestDB(t)
	result, _, err := queryParseData(db, "nonexistent", cursor{})
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}
	if result.slug != "" {
		t.Errorf("expected empty result for missing session, got slug=%q", result.slug)
	}
}

func TestQueryParseDataTerminal(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-archived", "done", "/project",
		`{"id":"model"}`, 8000, 500,
		"2026-01-15T10:00:00Z", "2026-01-15T11:00:00Z", "2026-01-15T11:00:00Z")

	result, _, err := queryParseData(db, "sess-archived", cursor{})
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}
	if !result.terminal {
		t.Error("expected terminal session")
	}
	if result.archivedAt.IsZero() {
		t.Error("expected non-zero archivedAt")
	}
}

func TestQueryParseDataNoMessages(t *testing.T) {
	db := createTestDB(t)
	seedSession(t, db, "sess-empty", "empty", "/project",
		`{"id":"model"}`, 0, 0,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")

	result, _, err := queryParseData(db, "sess-empty", cursor{})
	if err != nil {
		t.Fatalf("queryParseData: %v", err)
	}
	if result.msgDelta != 0 {
		t.Errorf("msgDelta = %d, want 0", result.msgDelta)
	}
	if result.toolDelta != 0 {
		t.Errorf("toolDelta = %d, want 0", result.toolDelta)
	}
	if result.currentTool != "" {
		t.Errorf("currentTool = %q, want empty", result.currentTool)
	}
	if result.lastActivity.IsZero() {
		t.Error("expected lastActivity from session updated_at")
	}
}

// ---- test helpers -----------------------------------------------------------

const createSchema = `
CREATE TABLE session (
	id TEXT PRIMARY KEY,
	project_id TEXT,
	workspace_id TEXT,
	parent_id TEXT,
	slug TEXT,
	directory TEXT,
	path TEXT,
	title TEXT,
	agent TEXT,
	model TEXT,
	cost REAL DEFAULT 0,
	tokens_input INTEGER DEFAULT 0,
	tokens_output INTEGER DEFAULT 0,
	tokens_reasoning INTEGER DEFAULT 0,
	tokens_cache_read INTEGER DEFAULT 0,
	tokens_cache_write INTEGER DEFAULT 0,
	time_compacting TEXT,
	time_archived TEXT,
	summary TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE message (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	data TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE part (
	id TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	data TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`

func createTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(createSchema); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedSession(t *testing.T, db *sql.DB, id, slug, directory, model string,
	tokensIn, tokensOut int, createdAt, updatedAt, timeArchived string) {
	t.Helper()
	var archived sql.NullString
	if timeArchived != "" {
		archived = sql.NullString{String: timeArchived, Valid: true}
	}
	_, err := db.Exec(
		`INSERT INTO session (id, slug, directory, model, tokens_input, tokens_output, created_at, updated_at, time_archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, slug, directory, model, tokensIn, tokensOut, createdAt, updatedAt, archived,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func seedMessage(t *testing.T, db *sql.DB, id, sessionID, data, createdAt, updatedAt string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO message (id, session_id, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, data, createdAt, updatedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func seedPart(t *testing.T, db *sql.DB, id, messageID, sessionID, data, createdAt, updatedAt string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO part (id, message_id, session_id, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, data, createdAt, updatedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}
