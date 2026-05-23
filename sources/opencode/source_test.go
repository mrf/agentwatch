package opencode_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/opencode"

	_ "modernc.org/sqlite"
)

// ---- Name -------------------------------------------------------------------

func TestSourceName(t *testing.T) {
	src := opencode.New()
	if src.Name() != "opencode" {
		t.Errorf("Name() = %q, want %q", src.Name(), "opencode")
	}
}

// ---- Discover ---------------------------------------------------------------

func TestDiscoverNoDBPath(t *testing.T) {
	src := opencode.New()
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverMissingDB(t *testing.T) {
	src := opencode.New(opencode.WithDBPath("/nonexistent/path/opencode.db"))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverFindsSession(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "my-slug", "/home/user/project",
			`{"id":"claude-sonnet-4-20250514"}`, 1000, 200,
			"2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z", "")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(handles))
	}
	if handles[0].ID != "sess-1" {
		t.Errorf("ID = %q, want %q", handles[0].ID, "sess-1")
	}
	if handles[0].Source != "opencode" {
		t.Errorf("Source = %q, want %q", handles[0].Source, "opencode")
	}
	if handles[0].Path != dbPath {
		t.Errorf("Path = %q, want %q", handles[0].Path, dbPath)
	}
	if handles[0].WorkingDir != "/home/user/project" {
		t.Errorf("WorkingDir = %q, want %q", handles[0].WorkingDir, "/home/user/project")
	}
}

func TestDiscoverMultipleSessions(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "alpha", "/a",
			`{"id":"model"}`, 0, 0,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertSession(t, db, "sess-2", "beta", "/b",
			`{"id":"model"}`, 0, 0,
			"2026-01-15T11:00:00Z", "2026-01-15T11:00:00Z", "")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("expected 2 handles, got %d", len(handles))
	}
}

func TestDiscoverWindowFiltersOld(t *testing.T) {
	recentTime := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "old-sess", "old", "/old",
			`{"id":"model"}`, 0, 0,
			"2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z", "")
		insertSession(t, db, "new-sess", "new", "/new",
			`{"id":"model"}`, 0, 0,
			recentTime, recentTime, "")
	})

	src := opencode.New(opencode.WithDBPath(dbPath), opencode.WithDiscoverWindow(24*time.Hour))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	// Only the "new" session should survive the window filter.
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle after window filter, got %d", len(handles))
	}
	if handles[0].ID != "new-sess" {
		t.Errorf("ID = %q, want %q", handles[0].ID, "new-sess")
	}
}

// ---- Register ---------------------------------------------------------------

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	if err := opencode.Register(r); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "opencode" {
		t.Errorf("Names() = %v, want [opencode]", names)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := source.NewRegistry()
	if err := opencode.Register(r); err != nil {
		t.Fatalf("first Register error: %v", err)
	}
	if err := opencode.Register(r); err == nil {
		t.Error("expected error on duplicate Register, got nil")
	}
}

// ---- Parse ------------------------------------------------------------------

func TestParseCursorAdvances(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "slug", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if c1 == "" {
		t.Error("cursor should not be empty after reading content")
	}

	// Second parse with same cursor yields no new deltas.
	u2, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error on second call: %v", err)
	}
	if c2 != c1 {
		t.Errorf("cursor changed on re-parse: %v vs %v", c2, c1)
	}
	if u2.MessageCountDelta != 0 {
		t.Errorf("MessageCountDelta = %d, want 0 on re-parse", u2.MessageCountDelta)
	}
}

func TestParseInvalidCursor(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 0, 0,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	_, _, err := src.Parse(context.Background(), h, source.Cursor("not-valid"))
	if err == nil {
		t.Error("expected error for invalid cursor")
	}
}

func TestParseSessionData(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "build-it", "/home/user/project",
			`{"id":"claude-sonnet-4-20250514","providerID":"anthropic"}`, 5000, 200,
			"2026-01-15T10:00:00Z", "2026-01-15T10:30:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user","parts":[]}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
		insertMessage(t, db, "msg-2", "sess-1", `{"role":"assistant","parts":[]}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
		insertPart(t, db, "part-1", "msg-2", "sess-1",
			`{"type":"tool-invocation","toolName":"Bash","state":"completed"}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertEqual(t, "SessionID", u.SessionID, "sess-1")
	assertEqual(t, "Slug", u.Slug, "build-it")
	assertEqual(t, "Model", u.Model, "claude-sonnet-4-20250514")
	assertEqual(t, "WorkingDir", u.WorkingDir, "/home/user/project")
	assertInt(t, "ContextTokens", u.ContextTokens, 5000)
	assertInt(t, "OutputTokens", u.OutputTokens, 200)
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 2)
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 1)
	assertEqual(t, "CurrentTool", u.CurrentTool, "Bash")
}

func TestParseTerminalSession(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-done", "done", "/project",
			`{"id":"model"}`, 8000, 500,
			"2026-01-15T10:00:00Z", "2026-01-15T11:00:00Z", "2026-01-15T11:00:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-done", Path: dbPath, Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !u.Terminal {
		t.Error("expected Terminal = true")
	}
	if u.Activity != session.ActivityTerminal {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityTerminal)
	}
	assertEqual(t, "EndReason", u.EndReason, "archived")
	if u.EndedAt.IsZero() {
		t.Error("expected non-zero EndedAt")
	}
}

func TestParseActivityWaiting(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u.Activity != session.ActivityWaiting {
		t.Errorf("Activity = %q, want %q (user message, no tools)", u.Activity, session.ActivityWaiting)
	}
}

func TestParseActivityWorking(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:01:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
		insertMessage(t, db, "msg-2", "sess-1", `{"role":"assistant"}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
		insertPart(t, db, "part-1", "msg-2", "sess-1",
			`{"type":"tool-invocation","toolName":"Edit"}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want %q (tool calls present)", u.Activity, session.ActivityWorking)
	}
}

func TestParseActivityIdle(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	// First parse sees the message.
	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Second parse: no new data -> idle.
	u2, _, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u2.Activity != session.ActivityIdle {
		t.Errorf("Activity = %q, want %q (no new data)", u2.Activity, session.ActivityIdle)
	}
}

func TestParseDelta(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:05:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	// First parse: 1 message.
	u1, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "first MessageCountDelta", u1.MessageCountDelta, 1)
	assertInt(t, "first ToolCallCountDelta", u1.ToolCallCountDelta, 0)

	// Add another message and a tool call to the DB.
	addDataToDB(t, dbPath, func(db *sql.DB) {
		insertMessage(t, db, "msg-2", "sess-1", `{"role":"assistant"}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
		insertPart(t, db, "part-1", "msg-2", "sess-1",
			`{"type":"tool-invocation","toolName":"Grep"}`,
			"2026-01-15T10:01:00Z", "2026-01-15T10:01:00Z")
	})

	// Second parse: delta should be 1 message, 1 tool call.
	u2, _, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "second MessageCountDelta", u2.MessageCountDelta, 1)
	assertInt(t, "second ToolCallCountDelta", u2.ToolCallCountDelta, 1)
	assertEqual(t, "CurrentTool", u2.CurrentTool, "Grep")
}

func TestParseMissingSession(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		// Empty DB with schema but no sessions.
	})

	src := opencode.New(opencode.WithDBPath(dbPath))
	h := source.SessionHandle{ID: "nonexistent", Path: dbPath, Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Session not in DB: zero update with h.ID, no deltas.
	assertEqual(t, "SessionID", u.SessionID, "nonexistent")
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 0)
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 0)
}

func TestParseMissingDB(t *testing.T) {
	src := opencode.New(opencode.WithDBPath("/nonexistent/opencode.db"))
	h := source.SessionHandle{ID: "sess-1", Source: "opencode"}

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.SessionID != "" {
		t.Errorf("expected empty update for missing DB, got SessionID=%q", u.SessionID)
	}
}

func TestParseModelFormats(t *testing.T) {
	tests := []struct {
		name      string
		modelJSON string
		want      string
	}{
		{"object", `{"id":"gpt-4o","providerID":"openai"}`, "gpt-4o"},
		{"string", `"claude-sonnet"`, "claude-sonnet"},
		{"bare", `o3-mini`, "o3-mini"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := seedFileDB(t, func(db *sql.DB) {
				insertSession(t, db, "s", "", "/p", tt.modelJSON, 0, 0,
					"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
			})
			src := opencode.New(opencode.WithDBPath(dbPath))
			h := source.SessionHandle{ID: "s", Path: dbPath, Source: "opencode"}

			u, _, err := src.Parse(context.Background(), h, "")
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			assertEqual(t, "Model", u.Model, tt.want)
		})
	}
}

// ---- helpers ----------------------------------------------------------------

const testSchema = `
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

// seedFileDB creates a temp SQLite DB file with the schema and runs seed.
func seedFileDB(t *testing.T, seed func(db *sql.DB)) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(testSchema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	seed(db)
	_ = db.Close()
	return dbPath
}

// addDataToDB reopens an existing test DB and runs fn to add more data.
func addDataToDB(t *testing.T, dbPath string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fn(db)
}

func insertSession(t *testing.T, db *sql.DB, id, slug, directory, model string,
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

func insertMessage(t *testing.T, db *sql.DB, id, sessionID, data, createdAt, updatedAt string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO message (id, session_id, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, data, createdAt, updatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func insertPart(t *testing.T, db *sql.DB, id, messageID, sessionID, data, createdAt, updatedAt string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO part (id, message_id, session_id, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, data, createdAt, updatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func assertInt(t *testing.T, field string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}

// Ensure Source implements source.Source at compile time.
var _ source.Source = (*opencode.Source)(nil)
