package codex_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/codex"
)

// ---- Name ------------------------------------------------------------------

func TestSourceName(t *testing.T) {
	src := codex.New()
	if src.Name() != "codex" {
		t.Errorf("Name() = %q, want %q", src.Name(), "codex")
	}
}

// ---- Discover --------------------------------------------------------------

func TestDiscoverNoRoot(t *testing.T) {
	// Without WithRoot, Discover must return nil, nil.
	src := codex.New()
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverMissingSessionsDir(t *testing.T) {
	root := t.TempDir() // has no "sessions/" subdirectory
	src := codex.New(codex.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverFindsRolloutFiles(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "sessions", "2026", "01", "30")
	mustMkdir(t, sessDir)

	rollout := filepath.Join(sessDir, "rollout-1738000000-01234567-abcd-ef01-2345-67890abcdef0.jsonl")
	mustWriteFile(t, rollout, `{"type":"session_meta","payload":{"id":"01234567-abcd-ef01-2345-67890abcdef0"}}`)

	src := codex.New(codex.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(handles))
	}
	if handles[0].Source != "codex" {
		t.Errorf("Source = %q, want %q", handles[0].Source, "codex")
	}
	if handles[0].Path != rollout {
		t.Errorf("Path = %q, want %q", handles[0].Path, rollout)
	}
	if handles[0].ID != "01234567-abcd-ef01-2345-67890abcdef0" {
		t.Errorf("ID = %q, want UUID from filename", handles[0].ID)
	}
}

func TestDiscoverIgnoresNonRolloutFiles(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "sessions", "2026", "04", "24")
	mustMkdir(t, sessDir)

	// Non-rollout files should be ignored.
	mustWriteFile(t, filepath.Join(sessDir, "history.jsonl"), `{}`)
	mustWriteFile(t, filepath.Join(sessDir, "rollout-abc-uuid.txt"), `{}`)

	// Valid rollout.
	mustWriteFile(t, filepath.Join(sessDir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl"), `{}`)

	src := codex.New(codex.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle (only the .jsonl rollout), got %d", len(handles))
	}
}

func TestDiscoverDiscoverWindowFiltersOldFiles(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "sessions", "2020", "01", "01")
	mustMkdir(t, sessDir)

	old := filepath.Join(sessDir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl")
	mustWriteFile(t, old, `{}`)
	// Force old mtime.
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	src := codex.New(codex.WithRoot(root), codex.WithDiscoverWindow(24*time.Hour))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles after age filter, got %d", len(handles))
	}
}

// ---- Register --------------------------------------------------------------

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	root := t.TempDir()
	if err := codex.Register(r, codex.WithRoot(root)); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "codex" {
		t.Errorf("Names() = %v, want [codex]", names)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := source.NewRegistry()
	if err := codex.Register(r); err != nil {
		t.Fatalf("first Register error: %v", err)
	}
	if err := codex.Register(r); err == nil {
		t.Error("expected error on duplicate Register, got nil")
	}
}

// ---- Parse: cursor mechanics -----------------------------------------------

func TestParseCursorAdvances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl")
	mustWriteFile(t, path, newEnvelopeFixture)

	src := codex.New(codex.WithRoot(dir))
	h := source.SessionHandle{ID: "01234567-abcd-ef01-2345-67890abcdef0", Path: path, Source: "codex"}

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if c1 == "" {
		t.Error("cursor should not be empty after reading content")
	}

	// Second parse with cursor should yield no new data and same cursor.
	_, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error on second call: %v", err)
	}
	if c2 != c1 {
		t.Errorf("cursor changed on re-parse: %v vs %v", c2, c1)
	}
}

func TestParseInvalidCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl")
	mustWriteFile(t, path, `{}`)

	src := codex.New(codex.WithRoot(dir))
	h := source.SessionHandle{ID: "x", Path: path, Source: "codex"}

	_, _, err := src.Parse(context.Background(), h, source.Cursor("not-a-number"))
	if err == nil {
		t.Error("expected error for invalid cursor")
	}
}

// ---- Parse: new envelope format --------------------------------------------

// newEnvelopeFixture exercises the current rollout RolloutLine envelope format.
const newEnvelopeFixture = `{"type":"session_meta","payload":{"id":"01234567-abcd-ef01-2345-67890abcdef0","model":"o4-mini","timestamp":"2026-01-30T10:00:00.000Z"}}
{"type":"env_context","payload":{"cwd":"/home/user/project"}}
{"type":"event_msg","payload":{"type":"user_message","payload":{"text":"fix the bug"}}}
{"type":"event_msg","payload":{"type":"agent_message","payload":{"text":"on it"}}}
{"type":"response_item","payload":{"type":"command_execution","command":"grep -r TODO src/"}}
{"type":"event_msg","payload":{"type":"token_count","payload":{"input_tokens":5000,"output_tokens":200,"model_context_window":128000}}}
`

func TestParseNewEnvelopeFormat(t *testing.T) {
	dir, h := writeRollout(t, "01234567-abcd-ef01-2345-67890abcdef0", newEnvelopeFixture)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// session_meta ID
	assertEqual(t, "SessionID", u.SessionID, "01234567-abcd-ef01-2345-67890abcdef0")
	// model from session_meta
	assertEqual(t, "Model", u.Model, "o4-mini")
	// cwd from env_context
	assertEqual(t, "WorkingDir", u.WorkingDir, "/home/user/project")
	// 1 user_message + 1 agent_message
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 2)
	// 1 command_execution
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 1)
	assertEqual(t, "CurrentTool", u.CurrentTool, "Bash")
	// flat token_count
	assertInt(t, "ContextTokens", u.ContextTokens, 5000)
	assertInt(t, "OutputTokens", u.OutputTokens, 200)
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 128000)
}

func TestParseNewEnvelopeSessionMetaIDField(t *testing.T) {
	// session_meta uses "id" (not "session_id") in real Codex CLI files.
	content := `{"type":"session_meta","payload":{"id":"real-id","model":"gpt-5.4","timestamp":"2026-04-24T20:10:05.106Z"}}
{"type":"event_msg","payload":{"type":"user_message","payload":{"text":"hello"}}}
`
	dir, h := writeRollout(t, "real-id", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertEqual(t, "SessionID", u.SessionID, "real-id")
	assertEqual(t, "Model", u.Model, "gpt-5.4")
}

func TestParseTurnContext(t *testing.T) {
	content := `{"type":"session_meta","payload":{"id":"tc-sess","model":"gpt-5.2-codex"}}
{"type":"turn_context","payload":{"cwd":"/home/user/myproject","model":"gpt-5.2-codex"}}
`
	dir, h := writeRollout(t, "tc-sess", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertEqual(t, "WorkingDir", u.WorkingDir, "/home/user/myproject")
	assertEqual(t, "Model", u.Model, "gpt-5.2-codex")
}

func TestParseSessionConfiguredOverridesModel(t *testing.T) {
	content := `{"type":"session_meta","payload":{"id":"sc-sess","model":"o3"}}
{"type":"response_item","payload":{"type":"message","text":"hello"}}
{"type":"event_msg","payload":{"type":"session_configured","payload":{"model":"o4-mini"}}}
`
	dir, h := writeRollout(t, "sc-sess", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertEqual(t, "Model", u.Model, "o4-mini")
}

func TestParseTaskStartedContextWindow(t *testing.T) {
	content := `{"type":"session_meta","payload":{"id":"task-started","model":"gpt-5.4"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","model_context_window":258400}}
`
	dir, h := writeRollout(t, "task-started", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 258400)
}

func TestParseResponseItemVariants(t *testing.T) {
	content := `{"type":"session_meta","payload":{"id":"ri-test","model":"o3"}}
{"type":"response_item","payload":{"type":"message","text":"hello"}}
{"type":"response_item","payload":{"type":"reasoning","text":"thinking"}}
{"type":"response_item","payload":{"type":"web_search","query":"test"}}
{"type":"response_item","payload":{"type":"file_change","path":"a.go"}}
{"type":"response_item","payload":{"type":"mcp_tool_call","tool_name":"slack_send"}}
{"type":"response_item","payload":{"type":"function_call","name":"shell_command"}}
`
	dir, h := writeRollout(t, "ri-test", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// message → 1 message
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
	// web_search + file_change + mcp_tool_call + function_call = 4 tool calls
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 4)
	// last tool call was function_call "shell_command"
	assertEqual(t, "CurrentTool", u.CurrentTool, "shell_command")
}

func TestParseActivityMapping(t *testing.T) {
	content := `{"type":"session_meta","payload":{"id":"act-test","model":"o3"}}
{"type":"event_msg","payload":{"type":"user_message","payload":{"text":"go"}}}
`
	dir, h := writeRollout(t, "act-test", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u.Activity != session.ActivityWaiting {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWaiting)
	}
}

// ---- Parse: nested token format (newer Codex CLI) --------------------------

// nestedTokenFixture is a real-format excerpt showing token data nested under
// info.last_token_usage and info.total_token_usage.
const nestedTokenFixture = `{"timestamp":"2026-03-08T00:41:42.190Z","type":"session_meta","payload":{"id":"fixture-last-token-usage","timestamp":"2026-03-08T00:41:42.190Z","cwd":"/workspace/project","originator":"codex_cli_rs","cli_version":"0.111.0","source":"cli","model_provider":"openai","model":"gpt-5.4"}}
{"timestamp":"2026-03-08T00:41:42.190Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","model_context_window":258400,"collaboration_mode_kind":"default"}}
{"timestamp":"2026-03-08T00:41:42.194Z","type":"turn_context","payload":{"turn_id":"turn-1","cwd":"/workspace/project","current_date":"2026-03-07","timezone":"America/Los_Angeles","approval_policy":"on-request","model":"gpt-5.4","effort":"high"}}
{"timestamp":"2026-03-08T00:41:57.923Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":64346294,"cached_input_tokens":61355392,"output_tokens":161772,"reasoning_output_tokens":59756,"total_tokens":64508066},"last_token_usage":{"input_tokens":194819,"cached_input_tokens":141696,"output_tokens":546,"reasoning_output_tokens":302,"total_tokens":195365},"model_context_window":258400},"rate_limits":{"limit_id":"codex","primary":{"used_percent":19.0,"window_minutes":300},"secondary":{"used_percent":6.0,"window_minutes":10080}}}}
`

func TestParseNestedTokenFormatPrefersLastTokenUsage(t *testing.T) {
	dir, h := writeRollout(t, "fixture-last-token-usage", nestedTokenFixture)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Must use last_token_usage, not total_token_usage.
	assertInt(t, "ContextTokens", u.ContextTokens, 194819)
	assertInt(t, "OutputTokens", u.OutputTokens, 546)
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 258400)
	assertEqual(t, "Model", u.Model, "gpt-5.4")
	assertEqual(t, "WorkingDir", u.WorkingDir, "/workspace/project")
}

func TestParseNestedTokenWithoutLastUsageKeepsZero(t *testing.T) {
	// When last_token_usage is absent, tokens should be zero (not from total).
	content := `{"type":"session_meta","payload":{"id":"nested-test","model":"gpt-5.2-codex"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":45000,"output_tokens":1200},"model_context_window":258400},"rate_limits":{"primary":null}}}
`
	dir, h := writeRollout(t, "nested-test", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "ContextTokens (should be 0)", u.ContextTokens, 0)
	assertInt(t, "OutputTokens (should be 0)", u.OutputTokens, 0)
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 258400)
}

func TestParseNullInfoTokenCount(t *testing.T) {
	// info:null appears on the first token_count before any API call completes.
	content := `{"type":"session_meta","payload":{"id":"null-info","model":"gpt-5.2-codex"}}
{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":null}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":8000,"output_tokens":200},"model_context_window":258400},"rate_limits":{"primary":null}}}
`
	dir, h := writeRollout(t, "null-info", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "ContextTokens (should be 0)", u.ContextTokens, 0)
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 258400)
}

// ---- Parse: old bare format ------------------------------------------------

const oldFormatFixture = `{"session_id":"old-session-123","model":"gpt-5-codex","timestamp":"2026-01-30T10:00:00.000Z"}
{"type":"message","text":"I'll help you with that"}
{"type":"command_execution","command":"ls -la"}
{"type":"file_change","path":"src/main.go"}
`

func TestParseOldBareFormat(t *testing.T) {
	dir, h := writeRollout(t, "old-session-123", oldFormatFixture)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertEqual(t, "SessionID", u.SessionID, "old-session-123")
	assertEqual(t, "Model", u.Model, "gpt-5-codex")
	// message → 1
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
	// command_execution + file_change → 2
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 2)
}

func TestParseOldBareFormatAllToolTypes(t *testing.T) {
	content := `{"session_id":"tools-test","model":"o3","timestamp":"2026-01-30T10:00:00.000Z"}
{"type":"reasoning","text":"Let me think..."}
{"type":"web_search","query":"golang testing"}
{"type":"mcp_tool_call","tool_name":"database_query"}
{"type":"command_execution","command":"npm test"}
{"type":"file_change","path":"src/index.ts"}
{"type":"message","text":"Done"}
`
	dir, h := writeRollout(t, "tools-test", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// web_search + mcp_tool_call + command_execution + file_change = 4
	assertInt(t, "ToolCallCountDelta", u.ToolCallCountDelta, 4)
	assertInt(t, "MessageCountDelta", u.MessageCountDelta, 1)
}

func TestParseOldBareFormatFlatTokenCount(t *testing.T) {
	content := `{"session_id":"tc-test","model":"o3","timestamp":"2026-01-30T10:00:00.000Z"}
{"type":"token_count","input_tokens":3000,"output_tokens":100,"model_context_window":200000}
`
	dir, h := writeRollout(t, "tc-test", content)
	src := codex.New(codex.WithRoot(dir))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertInt(t, "ContextTokens", u.ContextTokens, 3000)
	assertInt(t, "OutputTokens", u.OutputTokens, 100)
	assertInt(t, "MaxContextTokens", u.MaxContextTokens, 200000)
}

// ---- Parse: oversized file / line ------------------------------------------

func TestParseOversizedFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"session_meta","payload":{"id":"huge-test","model":"o3"}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	const maxFile = 100 * 1024 * 1024
	if err := f.Truncate(maxFile + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	src := codex.New(codex.WithRoot(dir))
	h := source.SessionHandle{ID: "huge-test", Path: path, Source: "codex"}

	_, _, err = src.Parse(context.Background(), h, "")
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
}

func TestParseOversizedLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-0-01234567-abcd-ef01-2345-67890abcdef0.jsonl")

	bigValue := strings.Repeat("x", 1*1024*1024+10)
	oversized := fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","text":%q}}`+"\n", bigValue)
	normal := `{"type":"session_meta","payload":{"id":"bigline-test","model":"o3"}}` + "\n"
	after := `{"type":"env_context","payload":{"cwd":"/tmp/proj"}}` + "\n"

	mustWriteFile(t, path, normal+oversized+after)

	src := codex.New(codex.WithRoot(dir))
	h := source.SessionHandle{ID: "bigline-test", Path: path, Source: "codex"}

	// When a line exceeds the cap the entire ReadLines call returns an error.
	// The source surfaces it transparently.
	_, _, err := src.Parse(context.Background(), h, "")
	if err == nil {
		t.Fatal("expected error when a line exceeds the size cap")
	}
}

// ---- helpers ---------------------------------------------------------------

func writeRollout(t *testing.T, id, content string) (root string, h source.SessionHandle) {
	t.Helper()
	root = t.TempDir()
	sessDir := filepath.Join(root, "sessions", "2026", "01", "30")
	mustMkdir(t, sessDir)
	path := filepath.Join(sessDir, "rollout-0-"+id+".jsonl")
	mustWriteFile(t, path, content)
	h = source.SessionHandle{ID: id, Path: path, Source: "codex"}
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
