package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/pi"
)

// ---- Name ------------------------------------------------------------------

func TestSourceName(t *testing.T) {
	src := pi.New()
	if src.Name() != "pi" {
		t.Errorf("Name() = %q, want %q", src.Name(), "pi")
	}
}

// ---- Discover --------------------------------------------------------------

func TestDiscoverNoRoot(t *testing.T) {
	src := pi.New()
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverMissingSessionsDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nonexistent")
	src := pi.New(pi.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverFindsSessionFiles(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "--home-user-project--")
	mustMkdir(t, projDir)

	path := filepath.Join(projDir, "20240115T100000_abc12345.jsonl")
	mustWriteFile(t, path, sessionHeaderFixture("abc12345"))

	src := pi.New(pi.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(handles))
	}
	if handles[0].Source != "pi" {
		t.Errorf("Source = %q, want %q", handles[0].Source, "pi")
	}
	if handles[0].Path != path {
		t.Errorf("Path = %q, want %q", handles[0].Path, path)
	}
	if handles[0].ID != "abc12345" {
		t.Errorf("ID = %q, want %q", handles[0].ID, "abc12345")
	}
}

func TestDiscoverIgnoresNonJsonlFiles(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "--home-user-project--")
	mustMkdir(t, projDir)

	mustWriteFile(t, filepath.Join(projDir, "session.txt"), `{}`)
	mustWriteFile(t, filepath.Join(projDir, "notes.md"), `# notes`)

	// One valid JSONL file.
	mustWriteFile(t, filepath.Join(projDir, "20240115T100000_abc12345.jsonl"), sessionHeaderFixture("abc12345"))

	src := pi.New(pi.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle (only the .jsonl file), got %d", len(handles))
	}
}

func TestDiscoverWindowFiltersOldFiles(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "--home-user-project--")
	mustMkdir(t, projDir)

	old := filepath.Join(projDir, "20200101T000000_oldfile.jsonl")
	mustWriteFile(t, old, sessionHeaderFixture("oldfile"))

	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	src := pi.New(pi.WithRoot(root), pi.WithDiscoverWindow(24*time.Hour))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles after age filter, got %d", len(handles))
	}
}

func TestDiscoverMultipleProjectDirs(t *testing.T) {
	root := t.TempDir()

	for _, dir := range []string{"--home-user-proj1--", "--home-user-proj2--"} {
		d := filepath.Join(root, dir)
		mustMkdir(t, d)
		mustWriteFile(t, filepath.Join(d, "20240115T100000_"+dir+".jsonl"), sessionHeaderFixture(dir))
	}

	src := pi.New(pi.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 2 {
		t.Errorf("expected 2 handles, got %d", len(handles))
	}
}

// ---- Register --------------------------------------------------------------

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	root := t.TempDir()
	if err := pi.Register(r, pi.WithRoot(root)); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "pi" {
		t.Errorf("Names() = %v, want [pi]", names)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := source.NewRegistry()
	if err := pi.Register(r); err != nil {
		t.Fatalf("first Register error: %v", err)
	}
	if err := pi.Register(r); err == nil {
		t.Error("expected error on duplicate Register, got nil")
	}
}

// ---- Parse: incremental SessionID ------------------------------------------

// TestParseIncrementalSessionID reproduces the bug where an incremental parse
// (offset > 0) returns an empty SessionID because the "session" header line is
// not re-read. The monitor treats SessionID == "" as "no new data" and skips
// the update, so any new lines appended after the first parse are silently
// dropped.
func TestParseIncrementalSessionID(t *testing.T) {
	initialContent := `{"type":"session","id":"inc001","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"inc001","timestamp":"2024-01-15T10:00:05Z","message":{"role":"user","content":"Hello"}}
`
	root, h := writeSession(t, "inc001", initialContent)
	src := pi.New(pi.WithRoot(root))

	// First parse: reads from offset 0, session header is present.
	u1, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse error: %v", err)
	}
	if u1.SessionID != "inc001" {
		t.Errorf("first parse SessionID = %q, want %q", u1.SessionID, "inc001")
	}

	// Append a new message to simulate ongoing session activity.
	newLine := `{"type":"message","id":"m2","parentId":"m1","timestamp":"2024-01-15T10:00:10Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":100,"output":20},"stopReason":"end_turn"}}` + "\n"
	f, err := os.OpenFile(h.Path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open file for append: %v", err)
	}
	if _, err := f.WriteString(newLine); err != nil {
		f.Close()
		t.Fatalf("append line: %v", err)
	}
	f.Close()

	// Second parse: offset > 0, session header is NOT re-read.
	// Bug: acc.sessionID stays empty → update.SessionID == "" → monitor skips it.
	u2, _, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse error: %v", err)
	}
	if u2.SessionID != "inc001" {
		t.Errorf("incremental parse SessionID = %q, want %q", u2.SessionID, "inc001")
	}
	if u2.MessageCountDelta != 1 {
		t.Errorf("incremental parse MessageCountDelta = %d, want 1", u2.MessageCountDelta)
	}
}

// ---- Parse: cursor mechanics -----------------------------------------------

func TestParseCursorAdvances(t *testing.T) {
	root, h := writeSession(t, "abc12345", fullSessionFixture)

	src := pi.New(pi.WithRoot(root))

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if c1 == "" {
		t.Error("cursor should not be empty after reading content")
	}

	// Second parse with cursor should return same cursor (no new data).
	_, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error on second call: %v", err)
	}
	if c2 != c1 {
		t.Errorf("cursor changed on re-parse: %v vs %v", c2, c1)
	}
}

func TestParseInvalidCursor(t *testing.T) {
	root, h := writeSession(t, "abc12345", sessionHeaderFixture("abc12345"))

	src := pi.New(pi.WithRoot(root))

	_, _, err := src.Parse(context.Background(), h, source.Cursor("not-a-number"))
	if err == nil {
		t.Error("expected error for invalid cursor")
	}
}

// ---- Parse: content --------------------------------------------------------

func TestParseSessionHeader(t *testing.T) {
	root, h := writeSession(t, "sess001", `{"type":"session","id":"sess001","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/home/user/proj"}
`)

	src := pi.New(pi.WithRoot(root))
	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertEqual(t, "SessionID", u.SessionID, "sess001")
	assertEqual(t, "WorkingDir", u.WorkingDir, "/home/user/proj")
	if u.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
}

func TestParseUserMessage(t *testing.T) {
	content := `{"type":"session","id":"usermsg","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"usermsg","timestamp":"2024-01-15T10:00:05Z","message":{"role":"user","content":"Hello"}}
`
	root, h := writeSession(t, "usermsg", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
	if u.Activity != session.ActivityWaiting {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWaiting)
	}
}

func TestParseAssistantMessage(t *testing.T) {
	content := `{"type":"session","id":"asst001","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"asst001","timestamp":"2024-01-15T10:00:05Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":300,"output":80,"cacheRead":20,"cacheWrite":10},"stopReason":"end_turn"}}
`
	root, h := writeSession(t, "asst001", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
	assertEqual(t, "Model", u.Model, "claude-opus-4-6")
	// contextTokens = 300 + 20 + 10 = 330
	assertInt(t, "ContextTokens", u.ContextTokens, 330)
	assertInt(t, "OutputTokens", u.OutputTokens, 80)
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWorking)
	}
}

func TestParseToolResult(t *testing.T) {
	content := `{"type":"session","id":"toolres","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"toolres","timestamp":"2024-01-15T10:00:05Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"read_file","content":"file data","isError":false}}
`
	root, h := writeSession(t, "toolres", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 1)
	assertEqual(t, "CurrentTool", u.CurrentTool, "read_file")
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWorking)
	}
}

func TestParseBashExecution(t *testing.T) {
	content := `{"type":"session","id":"bashex","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"bashex","timestamp":"2024-01-15T10:00:05Z","message":{"role":"bashExecution","command":"go test ./...","output":"ok\n","exitCode":0}}
`
	root, h := writeSession(t, "bashex", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 1)
	assertEqual(t, "CurrentTool", u.CurrentTool, "Bash")
}

func TestParseModelChange(t *testing.T) {
	content := `{"type":"session","id":"modchg","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
{"type":"message","id":"m1","parentId":"modchg","timestamp":"2024-01-15T10:00:05Z","message":{"role":"assistant","model":"claude-3-5-sonnet-20241022","usage":{"input":100,"output":20}}}
{"type":"model_change","id":"mc1","parentId":"m1","timestamp":"2024-01-15T10:00:10Z","provider":"anthropic","modelId":"claude-opus-4-6"}
`
	root, h := writeSession(t, "modchg", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertEqual(t, "Model", u.Model, "claude-opus-4-6")
}

func TestParseMalformedLinesSkipped(t *testing.T) {
	content := `{"type":"session","id":"malfm","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/proj"}
not json
{"type":"message","id":"m1","parentId":"malfm","timestamp":"2024-01-15T10:00:05Z","message":{"role":"user","content":"hi"}}
`
	root, h := writeSession(t, "malfm", content)
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	assertEqual(t, "SessionID", u.SessionID, "malfm")
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
}

// ---- helpers ---------------------------------------------------------------

// sessionHeaderFixture returns a minimal Pi session header JSON line.
func sessionHeaderFixture(id string) string {
	return `{"type":"session","id":"` + id + `","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/home/user/project"}` + "\n"
}

// fullSessionFixture is a complete Pi session used for cursor tests.
const fullSessionFixture = `{"type":"session","id":"full001","version":3,"timestamp":"2024-01-15T10:00:00Z","workingDir":"/home/user/project"}
{"type":"message","id":"m1","parentId":"full001","timestamp":"2024-01-15T10:00:05Z","message":{"role":"user","content":"Hello"}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2024-01-15T10:00:10Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":100,"output":20},"stopReason":"end_turn"}}
`

// writeSession creates a temp directory tree with one Pi session file and
// returns the root path and a SessionHandle pointing to it.
func writeSession(t *testing.T, id, content string) (root string, h source.SessionHandle) {
	t.Helper()
	root = t.TempDir()
	projDir := filepath.Join(root, "--home-user-project--")
	mustMkdir(t, projDir)
	path := filepath.Join(projDir, "20240115T100000_"+id+".jsonl")
	mustWriteFile(t, path, content)
	h = source.SessionHandle{ID: id, Path: path, Source: "pi"}
	return root, h
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
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
