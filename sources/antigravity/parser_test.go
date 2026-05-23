package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// bumpMtime advances the mtime of path by 1 second so that cursor-based
// change detection fires regardless of filesystem mtime resolution.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat for bumpMtime: %v", err)
	}
	future := fi.ModTime().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// fixtureData reads a testdata fixture file.
func fixtureData(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// --- parseSession unit tests ---

func TestParseSession_SimpleConversation(t *testing.T) {
	data := fixtureData(t, "simple.json")
	r, err := parseSession(data)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}

	if r.model != "gemini-2.5-pro" {
		t.Errorf("model: got %q, want %q", r.model, "gemini-2.5-pro")
	}
	if r.totalUserMsgs != 1 {
		t.Errorf("totalUserMsgs: got %d, want 1", r.totalUserMsgs)
	}
	if r.totalModelMsgs != 1 {
		t.Errorf("totalModelMsgs: got %d, want 1", r.totalModelMsgs)
	}
	if r.totalToolCalls != 0 {
		t.Errorf("totalToolCalls: got %d, want 0", r.totalToolCalls)
	}
	if r.contextTokens != 20 {
		t.Errorf("contextTokens: got %d, want 20", r.contextTokens)
	}
	if r.outputTokens != 12 {
		t.Errorf("outputTokens: got %d, want 12", r.outputTokens)
	}
	// Last turn is model text — session is waiting for user.
	if r.activity != session.ActivityWaiting {
		t.Errorf("activity: got %q, want %q", r.activity, session.ActivityWaiting)
	}
}

func TestParseSession_ToolSession(t *testing.T) {
	data := fixtureData(t, "tool_session.json")
	r, err := parseSession(data)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}

	if r.model != "gemini-2.5-flash" {
		t.Errorf("model: got %q, want %q", r.model, "gemini-2.5-flash")
	}
	// user text, model w/ tool, user functionResponse, model text
	if r.totalUserMsgs != 1 {
		t.Errorf("totalUserMsgs: got %d, want 1", r.totalUserMsgs)
	}
	if r.totalModelMsgs != 2 {
		t.Errorf("totalModelMsgs: got %d, want 2", r.totalModelMsgs)
	}
	if r.totalToolCalls != 1 {
		t.Errorf("totalToolCalls: got %d, want 1", r.totalToolCalls)
	}
	if r.currentTool != "list_directory" {
		t.Errorf("currentTool: got %q, want %q", r.currentTool, "list_directory")
	}
	// Last turn is model text — waiting for user input.
	if r.activity != session.ActivityWaiting {
		t.Errorf("activity: got %q, want %q", r.activity, session.ActivityWaiting)
	}
	// Token counts come from the last model turn's usageMetadata.
	if r.contextTokens != 150 {
		t.Errorf("contextTokens: got %d, want 150", r.contextTokens)
	}
	if r.outputTokens != 20 {
		t.Errorf("outputTokens: got %d, want 20", r.outputTokens)
	}
}

func TestParseSession_InProgressUserTurn(t *testing.T) {
	// A conversation ending with a user text turn means the model is processing.
	data := fixtureData(t, "in_progress.json")
	r, err := parseSession(data)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}

	if r.totalUserMsgs != 1 {
		t.Errorf("totalUserMsgs: got %d, want 1", r.totalUserMsgs)
	}
	if r.totalModelMsgs != 0 {
		t.Errorf("totalModelMsgs: got %d, want 0", r.totalModelMsgs)
	}
	if r.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", r.activity, session.ActivityWorking)
	}
}

func TestParseSession_Empty(t *testing.T) {
	r, err := parseSession([]byte(`{"messages":[], "model": "gemini-2.5-pro"}`))
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if r.totalUserMsgs != 0 || r.totalModelMsgs != 0 {
		t.Errorf("expected zero counts for empty history, got user=%d model=%d", r.totalUserMsgs, r.totalModelMsgs)
	}
	if r.activity != session.ActivityIdle {
		t.Errorf("activity: got %q, want ActivityIdle", r.activity)
	}
}

func TestParseSession_MalformedJSON(t *testing.T) {
	_, err := parseSession([]byte(`not valid json`))
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

func TestParseSession_FunctionResponseNotCountedAsUserMsg(t *testing.T) {
	// A user turn that only contains a functionResponse is not a user message.
	data := []byte(`{
		"messages": [
			{"role": "user", "parts": [{"text": "do something"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "read_file"}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "read_file"}}]}
		],
		"model": "gemini-2.5-pro"
	}`)
	r, err := parseSession(data)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	// Only the first user turn has text — the function response turn does not count.
	if r.totalUserMsgs != 1 {
		t.Errorf("totalUserMsgs: got %d, want 1 (functionResponse should not count)", r.totalUserMsgs)
	}
	// Last turn is user functionResponse: model is processing it.
	if r.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want ActivityWorking (processing tool result)", r.activity)
	}
}

func TestParseSession_ModelEndingWithToolCall(t *testing.T) {
	// If the last model turn ends with a functionCall, the model is still working.
	data := []byte(`{
		"messages": [
			{"role": "user", "parts": [{"text": "do something"}]},
			{"role": "model", "parts": [
				{"text": "I will read the file first."},
				{"functionCall": {"name": "read_file"}}
			]}
		],
		"model": "gemini-2.5-pro"
	}`)
	r, err := parseSession(data)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if r.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want ActivityWorking (last part is tool call)", r.activity)
	}
	if r.totalToolCalls != 1 {
		t.Errorf("totalToolCalls: got %d, want 1", r.totalToolCalls)
	}
}

// --- Parse (integration) tests ---

func newTestAntigravitySource(t *testing.T) *AntigravitySource {
	t.Helper()
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.(*AntigravitySource)
}

func TestParse_SimpleSession(t *testing.T) {
	s := newTestAntigravitySource(t)
	h := source.SessionHandle{
		ID:     "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		Path:   filepath.Join("testdata", "simple.json"),
		Source: "antigravity",
	}

	update, cursor, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cursor == "" {
		t.Error("cursor must not be empty after parsing data")
	}
	if update.SessionID != "a1b2c3d4-e5f6-7890-abcd-ef1234567890" {
		t.Errorf("SessionID: got %q, want %q", update.SessionID, "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	}
	if update.Model != "gemini-2.5-pro" {
		t.Errorf("Model: got %q, want %q", update.Model, "gemini-2.5-pro")
	}
	if update.Activity != session.ActivityWaiting {
		t.Errorf("Activity: got %q, want %q", update.Activity, session.ActivityWaiting)
	}
	// 1 user + 1 model = 2 messages on first parse
	if update.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta: got %d, want 2", update.MessageCountDelta)
	}
	if update.ToolCallCountDelta != 0 {
		t.Errorf("ToolCallCountDelta: got %d, want 0", update.ToolCallCountDelta)
	}
	if update.ContextTokens != 20 {
		t.Errorf("ContextTokens: got %d, want 20", update.ContextTokens)
	}
}

func TestParse_NoChangeNoUpdate(t *testing.T) {
	// Parsing the same file twice without modification returns empty update with same cursor.
	s := newTestAntigravitySource(t)
	h := source.SessionHandle{
		ID:     "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		Path:   filepath.Join("testdata", "simple.json"),
		Source: "antigravity",
	}

	_, c1, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}

	u2, c2, err := s.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if u2.SessionID != "" {
		t.Errorf("expected empty SourceUpdate on no-change, got SessionID=%q", u2.SessionID)
	}
	if c2 != c1 {
		t.Errorf("cursor changed with no file modification: %q -> %q", c1, c2)
	}
}

func TestParse_FileRewriteUpdatesState(t *testing.T) {
	// Write a file, parse it, then rewrite it with more content and parse again.
	// The second parse should return a delta for the new turns only.
	dir := t.TempDir()
	path := filepath.Join(dir, "019dc11d-218b-7ec1-a103-aeb16330d302.json")

	initial := `{
		"sessionId": "019dc11d-218b-7ec1-a103-aeb16330d302",
		"messages": [
			{"role": "user", "parts": [{"text": "hello"}]},
			{"role": "model", "parts": [{"text": "hi!"}], "usageMetadata": {"inputTokenCount": 10, "outputTokenCount": 5, "totalTokenCount": 15}}
		],
		"model": "gemini-2.5-pro"
	}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	s := newTestAntigravitySource(t)
	h := source.SessionHandle{ID: "sess1", Path: path, Source: "antigravity"}

	u1, c1, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if u1.MessageCountDelta != 2 {
		t.Errorf("first parse: MessageCountDelta got %d, want 2", u1.MessageCountDelta)
	}

	// Rewrite the file and advance its mtime by 1 second to guarantee detection.
	updated := `{
		"sessionId": "019dc11d-218b-7ec1-a103-aeb16330d302",
		"messages": [
			{"role": "user", "parts": [{"text": "hello"}]},
			{"role": "model", "parts": [{"text": "hi!"}], "usageMetadata": {"inputTokenCount": 10, "outputTokenCount": 5, "totalTokenCount": 15}},
			{"role": "user", "parts": [{"text": "what can you do?"}]},
			{"role": "model", "parts": [{"text": "I can help with many tasks."}], "usageMetadata": {"inputTokenCount": 30, "outputTokenCount": 15, "totalTokenCount": 45}}
		],
		"model": "gemini-2.5-pro"
	}`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	// Advance mtime by 1 second to guarantee the cursor changes regardless of
	// filesystem mtime resolution or scheduler jitter.
	bumpMtime(t, path)

	u2, c2, err := s.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if c2 == c1 {
		t.Error("cursor must change after file rewrite")
	}
	// Delta should only count the 2 new turns (1 user + 1 model).
	if u2.MessageCountDelta != 2 {
		t.Errorf("second parse: MessageCountDelta got %d, want 2 (only new turns)", u2.MessageCountDelta)
	}
	if u2.Activity != session.ActivityWaiting {
		t.Errorf("second parse: Activity got %q, want %q", u2.Activity, session.ActivityWaiting)
	}
	// Context tokens should reflect the latest model turn's usage.
	if u2.ContextTokens != 30 {
		t.Errorf("second parse: ContextTokens got %d, want 30", u2.ContextTokens)
	}
}

func TestParse_FileRewrite_ToolCallDelta(t *testing.T) {
	// Verify tool call deltas are computed correctly across rewrites.
	dir := t.TempDir()
	path := filepath.Join(dir, "019dc11d-218b-7ec1-a103-aeb16330d302.json")

	initial := `{
		"sessionId": "019dc11d-218b-7ec1-a103-aeb16330d302",
		"messages": [
			{"role": "user", "parts": [{"text": "list files"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "list_dir"}}]}
		],
		"model": "gemini-2.5-pro"
	}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	s := newTestAntigravitySource(t)
	h := source.SessionHandle{ID: "sess2", Path: path, Source: "antigravity"}

	u1, c1, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if u1.ToolCallCountDelta != 1 {
		t.Errorf("first parse: ToolCallCountDelta got %d, want 1", u1.ToolCallCountDelta)
	}

	updated := `{
		"sessionId": "019dc11d-218b-7ec1-a103-aeb16330d302",
		"messages": [
			{"role": "user", "parts": [{"text": "list files"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "list_dir"}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "list_dir"}}]},
			{"role": "model", "parts": [{"functionCall": {"name": "read_file"}}]}
		],
		"model": "gemini-2.5-pro"
	}`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write updated: %v", err)
	}
	bumpMtime(t, path)

	u2, _, err := s.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	// One new tool call (read_file), list_dir was already counted.
	if u2.ToolCallCountDelta != 1 {
		t.Errorf("second parse: ToolCallCountDelta got %d, want 1", u2.ToolCallCountDelta)
	}
	if u2.CurrentTool != "read_file" {
		t.Errorf("second parse: CurrentTool got %q, want %q", u2.CurrentTool, "read_file")
	}
}

func TestParse_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newTestAntigravitySource(t)
	h := source.SessionHandle{
		ID:   "any",
		Path: filepath.Join("testdata", "simple.json"),
	}
	_, _, err := s.Parse(ctx, h, "")
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

func TestParse_WorkingDirFromHandle(t *testing.T) {
	// WorkingDir set on the handle is propagated to the update.
	s := newTestAntigravitySource(t)
	h := source.SessionHandle{
		ID:         "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		Path:       filepath.Join("testdata", "simple.json"),
		WorkingDir: "/home/user/myproject",
		Source:     "antigravity",
	}
	update, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.WorkingDir != "/home/user/myproject" {
		t.Errorf("WorkingDir: got %q, want %q", update.WorkingDir, "/home/user/myproject")
	}
}

// --- Discover tests ---

func TestDiscover_FindsSessions(t *testing.T) {
	// Create a directory tree mirroring Antigravity CLI's ~/.antigravitycli/ layout:
	// root/<uuid1>.json
	// root/<uuid2>.json
	// root/notes.txt  (not a .json — should be ignored)
	root := t.TempDir()

	for _, uuid := range []string{
		"a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"b2c3d4e5-f6a7-8901-bcde-f12345678901",
	} {
		content := `{"messages":[], "model":"gemini-2.5-pro"}`
		if err := os.WriteFile(filepath.Join(root, uuid+".json"), []byte(content), 0o644); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}

	// Non-JSON file — should not appear.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(""), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	s, err := New(WithRoot(root))
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
		if handles[i].Source != "antigravity" {
			t.Errorf("handle[%d].Source: got %q, want %q", i, handles[i].Source, "antigravity")
		}
		if handles[i].ID == "" {
			t.Errorf("handle[%d].ID is empty", i)
		}
		// Path should point to a .json file
		if !filepath.IsAbs(handles[i].Path) || filepath.Ext(handles[i].Path) != ".json" {
			t.Errorf("handle[%d].Path: expected *.json, got %q", i, handles[i].Path)
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
	s, err := New(WithRoot("/nonexistent/antigravity/path"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
	content := `{"messages":[], "model":"gemini-2.5-pro"}`
	if err := os.WriteFile(filepath.Join(dir, "a1b2c3d4-e5f6-7890-abcd-ef1234567890.json"), []byte(content), 0o644); err != nil {
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
	if _, ok := r.Get("antigravity"); !ok {
		t.Error("antigravity not found in registry after Register")
	}
	// Double-register should fail.
	if err := Register(r); err == nil {
		t.Error("expected error on double Register, got nil")
	}
}
