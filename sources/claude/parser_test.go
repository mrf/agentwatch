package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// fixtureLines reads a testdata fixture and splits it into lines (without
// newlines) in the same way jsonl.ReadLines would. Used so parser unit tests
// don't depend on the file I/O path.
func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := make([]byte, i-start)
			copy(line, data[start:i])
			lines = append(lines, line)
			start = i + 1
		}
	}
	return lines
}

// --- parseLines unit tests ---

func TestParseLines_SimpleSession(t *testing.T) {
	lines := fixtureLines(t, "simple.jsonl")
	r := parseLines(lines)

	if r.sessionID != "aaaaaaaa-bbbb-cccc-dddd-000000000001" {
		t.Errorf("sessionID: got %q", r.sessionID)
	}
	if r.slug != "happy-little-session" {
		t.Errorf("slug: got %q", r.slug)
	}
	if r.model != "claude-opus-4-6" {
		t.Errorf("model: got %q", r.model)
	}
	if r.cwd != "/tmp/myproject" {
		t.Errorf("cwd: got %q", r.cwd)
	}
	if r.branch != "main" {
		t.Errorf("branch: got %q", r.branch)
	}
	// context tokens = input(15) + cache_read(0) + cache_creation(0) = 15
	if r.contextTokens != 15 {
		t.Errorf("contextTokens: got %d, want 15", r.contextTokens)
	}
	if r.outputTokens != 25 {
		t.Errorf("outputTokens: got %d, want 25", r.outputTokens)
	}
	// 1 user message + 1 assistant message
	if r.msgDelta != 2 {
		t.Errorf("msgDelta: got %d, want 2", r.msgDelta)
	}
	if r.toolDelta != 0 {
		t.Errorf("toolDelta: got %d, want 0", r.toolDelta)
	}
	if r.activity != session.ActivityWaiting {
		t.Errorf("activity: got %q, want %q", r.activity, session.ActivityWaiting)
	}
	if !r.hasData {
		t.Error("hasData: expected true")
	}
}

func TestParseLines_ToolSession(t *testing.T) {
	lines := fixtureLines(t, "tool_session.jsonl")
	r := parseLines(lines)

	if r.sessionID != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Errorf("sessionID: got %q", r.sessionID)
	}
	if r.model != "claude-sonnet-4-6" {
		t.Errorf("model: got %q", r.model)
	}
	// context tokens = input(100) + cache_read(50) + cache_creation(200) = 350
	if r.contextTokens != 350 {
		t.Errorf("contextTokens: got %d, want 350", r.contextTokens)
	}
	if r.outputTokens != 30 {
		t.Errorf("outputTokens: got %d, want 30", r.outputTokens)
	}
	if r.toolDelta != 1 {
		t.Errorf("toolDelta: got %d, want 1", r.toolDelta)
	}
	if r.currentTool != "Bash" {
		t.Errorf("currentTool: got %q, want %q", r.currentTool, "Bash")
	}
	if r.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", r.activity, session.ActivityWorking)
	}
}

func TestParseLines_ActivityWorking_AfterUserMsg(t *testing.T) {
	// A user message with no following assistant record should yield ActivityWorking.
	lines := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"go"}]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`),
	}
	r := parseLines(lines)
	if r.activity != session.ActivityWorking {
		t.Errorf("expected ActivityWorking after user msg, got %q", r.activity)
	}
}

func TestParseLines_StreamingChunkNotCounted(t *testing.T) {
	// An assistant record without a cwd field is a raw streaming chunk and must
	// not be counted as a completed turn.
	lines := [][]byte{
		// No cwd field — raw streaming event
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"thinking","thinking":"..."}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}},"uuid":"a1","timestamp":"2026-01-01T10:00:00.000Z"}`),
		// Same turn, enriched with cwd — counts once
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"Hi"},{"type":"tool_use","id":"t1","name":"Read","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}},"uuid":"a1","timestamp":"2026-01-01T10:00:01.000Z","cwd":"/proj","sessionId":"s1","gitBranch":"main","slug":"my-slug"}`),
	}
	r := parseLines(lines)
	if r.msgDelta != 1 {
		t.Errorf("msgDelta: got %d, want 1 (streaming chunk must not count)", r.msgDelta)
	}
	if r.toolDelta != 1 {
		t.Errorf("toolDelta: got %d, want 1", r.toolDelta)
	}
	if r.model != "claude-opus-4-6" {
		t.Errorf("model: got %q", r.model)
	}
}

func TestParseLines_MalformedLineSkipped(t *testing.T) {
	lines := [][]byte{
		[]byte(`not valid json`),
		[]byte(`{"type":"user","message":{"role":"user","content":[]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`),
	}
	r := parseLines(lines)
	if r.msgDelta != 1 {
		t.Errorf("msgDelta: got %d, want 1 (malformed line should be skipped)", r.msgDelta)
	}
}

func TestParseLines_EmptyLines(t *testing.T) {
	r := parseLines(nil)
	if r.hasData {
		t.Error("expected hasData=false for nil lines")
	}
	if r.activity != session.ActivityIdle {
		t.Errorf("expected ActivityIdle for empty input, got %q", r.activity)
	}
}

func TestParseLines_PermissionModeRecordIgnored(t *testing.T) {
	// A permission-mode record contributes sessionId but no message count.
	lines := [][]byte{
		[]byte(`{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`),
	}
	r := parseLines(lines)
	if r.sessionID != "s1" {
		t.Errorf("sessionID from permission-mode: got %q, want %q", r.sessionID, "s1")
	}
	if r.msgDelta != 0 {
		t.Errorf("msgDelta: got %d, want 0", r.msgDelta)
	}
	if r.hasData {
		t.Error("expected hasData=false for permission-mode only")
	}
}

func TestParseLines_Timestamps(t *testing.T) {
	lines := fixtureLines(t, "simple.jsonl")
	r := parseLines(lines)

	wantFirst := mustParseTime(t, "2026-01-01T10:00:00.000Z")
	wantLast := mustParseTime(t, "2026-01-01T10:00:02.000Z")

	if !r.startedAt.Equal(wantFirst) {
		t.Errorf("startedAt: got %v, want %v", r.startedAt, wantFirst)
	}
	if !r.lastActivityAt.Equal(wantLast) {
		t.Errorf("lastActivityAt: got %v, want %v", r.lastActivityAt, wantLast)
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// --- ClaudeSource integration tests (Parse + Discover) ---

func newTestSource(t *testing.T) *ClaudeSource {
	t.Helper()
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.(*ClaudeSource)
}

func TestParse_SimpleSession(t *testing.T) {
	s := newTestSource(t)
	h := source.SessionHandle{
		ID:     "aaaaaaaa-bbbb-cccc-dddd-000000000001",
		Path:   filepath.Join("testdata", "simple.jsonl"),
		Source: "claude",
	}

	update, cursor, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cursor == "" {
		t.Error("cursor must not be empty after parsing data")
	}
	if update.SessionID != "aaaaaaaa-bbbb-cccc-dddd-000000000001" {
		t.Errorf("SessionID: got %q", update.SessionID)
	}
	if update.Model != "claude-opus-4-6" {
		t.Errorf("Model: got %q", update.Model)
	}
	if update.ContextTokens != 15 {
		t.Errorf("ContextTokens: got %d, want 15", update.ContextTokens)
	}
	if update.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta: got %d, want 2", update.MessageCountDelta)
	}
	if update.Activity != session.ActivityWaiting {
		t.Errorf("Activity: got %q, want %q", update.Activity, session.ActivityWaiting)
	}
	if update.WorkingDir != "/tmp/myproject" {
		t.Errorf("WorkingDir: got %q", update.WorkingDir)
	}
	if update.Branch != "main" {
		t.Errorf("Branch: got %q", update.Branch)
	}
}

func TestParse_ToolSession(t *testing.T) {
	s := newTestSource(t)
	h := source.SessionHandle{
		ID:     "bbbbbbbb-0000-0000-0000-000000000002",
		Path:   filepath.Join("testdata", "tool_session.jsonl"),
		Source: "claude",
	}

	update, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.ToolCallCountDelta != 1 {
		t.Errorf("ToolCallCountDelta: got %d, want 1", update.ToolCallCountDelta)
	}
	if update.CurrentTool != "Bash" {
		t.Errorf("CurrentTool: got %q, want %q", update.CurrentTool, "Bash")
	}
	if update.Activity != session.ActivityWorking {
		t.Errorf("Activity: got %q, want %q", update.Activity, session.ActivityWorking)
	}
	if update.ContextTokens != 350 {
		t.Errorf("ContextTokens: got %d, want 350", update.ContextTokens)
	}
}

func TestParse_IncrementalCursor(t *testing.T) {
	// Write a temp file with two messages, parse incrementally.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	line1 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}` + "\n"
	line2 := `{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hey"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}},"uuid":"a1","timestamp":"2026-01-01T10:00:02.000Z","cwd":"/p","sessionId":"s1","gitBranch":"main","slug":"slug1"}` + "\n"
	line3 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"more"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:01:00.000Z","gitBranch":"main"}` + "\n"

	// First write: two lines.
	if err := os.WriteFile(path, []byte(line1+line2), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := newTestSource(t)
	h := source.SessionHandle{ID: "s1", Path: path, Source: "claude"}

	u1, c1, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if u1.MessageCountDelta != 2 {
		t.Errorf("first parse: MessageCountDelta got %d, want 2", u1.MessageCountDelta)
	}

	// Append a third line.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(line3); err != nil {
		f.Close()
		t.Fatalf("append line3: %v", err)
	}
	f.Close()

	u2, _, err := s.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	// Only the new user message should be counted.
	if u2.MessageCountDelta != 1 {
		t.Errorf("second parse: MessageCountDelta got %d, want 1", u2.MessageCountDelta)
	}
	if u2.Activity != session.ActivityWorking {
		t.Errorf("second parse: Activity got %q, want %q", u2.Activity, session.ActivityWorking)
	}
}

func TestParse_CursorNoNewData(t *testing.T) {
	// Parse once, then parse again with the returned cursor — should get empty update.
	s := newTestSource(t)
	h := source.SessionHandle{
		ID:     "aaaaaaaa-bbbb-cccc-dddd-000000000001",
		Path:   filepath.Join("testdata", "simple.jsonl"),
		Source: "claude",
	}

	_, c1, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}

	u2, c2, err := s.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if u2.SessionID != "" || u2.Model != "" {
		t.Errorf("expected empty SourceUpdate, got %+v", u2)
	}
	if c2 != c1 {
		t.Errorf("cursor changed with no new data: %q -> %q", c1, c2)
	}
}

func TestParse_SessionIDFallsBackToHandle(t *testing.T) {
	// If the JSONL has no sessionId field, Parse uses the handle ID.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"user","message":{"role":"user","content":[]},"cwd":"/x","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := newTestSource(t)
	h := source.SessionHandle{ID: "fallback-id", Path: path, Source: "claude"}
	u, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.SessionID != "fallback-id" {
		t.Errorf("SessionID fallback: got %q, want %q", u.SessionID, "fallback-id")
	}
}

func TestParse_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newTestSource(t)
	h := source.SessionHandle{
		ID:   "any",
		Path: filepath.Join("testdata", "simple.jsonl"),
	}
	_, _, err := s.Parse(ctx, h, "")
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

// --- Discover tests ---

func TestDiscover_FindsJSONLFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a nested project structure like ~/.claude/projects/<hash>/<uuid>.jsonl
	proj := filepath.Join(dir, "projects", "-home-user-code")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "aaa-bbb-ccc.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "ddd-eee-fff.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A non-JSONL file should be ignored.
	if err := os.WriteFile(filepath.Join(proj, "notes.txt"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(WithRoot(filepath.Join(dir, "projects")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 2 {
		t.Errorf("got %d handles, want 2", len(handles))
	}
	for i := 0; i < len(handles); i++ {
		if handles[i].Source != "claude" {
			t.Errorf("handle[%d].Source: got %q, want %q", i, handles[i].Source, "claude")
		}
		if handles[i].ID == "" {
			t.Errorf("handle[%d].ID is empty", i)
		}
	}
}

func TestDiscover_EmptyRoot(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected no handles for empty root, got %d", len(handles))
	}
}

func TestDiscover_NonExistentRoot(t *testing.T) {
	s, err := New(WithRoot("/nonexistent/path/that/does/not/exist"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Should not return an error for a missing directory.
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover returned error for missing root: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscover_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "session.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s, err := New(WithRoot(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Discover(ctx)
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Get("claude"); !ok {
		t.Error("claude not found in registry after Register")
	}
	// Double-register should fail.
	if err := Register(r); err == nil {
		t.Error("expected error on double Register, got nil")
	}
}
