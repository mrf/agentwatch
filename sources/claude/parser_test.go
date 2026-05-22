package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestParseLines_TokenCounting(t *testing.T) {
	// Verify that context tokens = input + cache_read + cache_creation
	lines := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":200,"output_tokens":30}},"uuid":"a1","timestamp":"2026-01-01T10:00:01.000Z","cwd":"/p","sessionId":"s1","gitBranch":"main","slug":"test"}`),
	}
	r := parseLines(lines)
	// context = 100 + 50 + 200 = 350
	if r.contextTokens != 350 {
		t.Errorf("contextTokens: got %d, want 350", r.contextTokens)
	}
	if r.outputTokens != 30 {
		t.Errorf("outputTokens: got %d, want 30", r.outputTokens)
	}
}

// --- Subagent tests ---

func TestParseLines_SubagentProgress(t *testing.T) {
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	if r.sessionID != "cccccccc-0000-0000-0000-000000000003" {
		t.Errorf("sessionID: got %q", r.sessionID)
	}

	// The parent session should have 3 messages: the user that triggered the
	// subagent, the assistant that called Task, and the user with the tool_result.
	if r.msgDelta != 3 {
		t.Errorf("msgDelta: got %d, want 3", r.msgDelta)
	}

	// Should have one subagent tracked.
	if len(r.subagents) != 1 {
		t.Fatalf("subagents: got %d, want 1", len(r.subagents))
	}

	sub := r.subagents["tu-agent-1"]
	if sub == nil {
		t.Fatal("expected subagent with toolUseID 'tu-agent-1'")
		return
	}
	if sub.model != "claude-sonnet-4-6" {
		t.Errorf("subagent model: got %q, want %q", sub.model, "claude-sonnet-4-6")
	}
	// Last progress entry had a tool_use (Read), so subagent should be working.
	if sub.activity != session.ActivityTerminal {
		// tool_result in the user message completes the subagent
		t.Logf("subagent activity: %q (completed subagent should be terminal)", sub.activity)
	}
}

func TestParseLines_SubagentCompletion(t *testing.T) {
	// Verify that a tool_result in a user message marks the subagent as completed.
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	sub := r.subagents["tu-agent-1"]
	if sub == nil {
		t.Fatal("expected subagent 'tu-agent-1'")
		return
	}
	if !sub.completed {
		t.Error("expected subagent to be marked completed after tool_result")
	}
	if sub.activity != session.ActivityTerminal {
		t.Errorf("completed subagent activity: got %q, want %q", sub.activity, session.ActivityTerminal)
	}
}

func TestParseLines_SubagentToolCalls(t *testing.T) {
	// The subagent in the fixture has a tool_use block (Read) in its second
	// progress entry — verify tool call count and current tool.
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	sub := r.subagents["tu-agent-1"]
	if sub == nil {
		t.Fatal("expected subagent 'tu-agent-1'")
		return
	}
	if sub.toolCalls != 1 {
		t.Errorf("subagent toolCalls: got %d, want 1", sub.toolCalls)
	}
	if sub.currentTool != "Read" {
		t.Errorf("subagent currentTool: got %q, want %q", sub.currentTool, "Read")
	}
}

func TestParseLines_SubagentTokens(t *testing.T) {
	// Verify subagent token counts from the progress entry.
	// Last progress entry: input=30, cache_read=0, cache_creation=0, output=8
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	sub := r.subagents["tu-agent-1"]
	if sub == nil {
		t.Fatal("expected subagent 'tu-agent-1'")
		return
	}
	if sub.contextTokens != 30 {
		t.Errorf("subagent contextTokens: got %d, want 30", sub.contextTokens)
	}
	if sub.outputTokens != 8 {
		t.Errorf("subagent outputTokens: got %d, want 8", sub.outputTokens)
	}
}

func TestParseLines_SubagentNotCountedAsSessionMsg(t *testing.T) {
	// Progress entries must not increment the parent session's message delta.
	// user(1) + assistant(1) + tool_result_user(1) = 3. Progress entries add 0.
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	if r.msgDelta != 3 {
		t.Errorf("msgDelta: got %d, want 3 (user+assistant+tool_result_user, no progress)", r.msgDelta)
	}
}

func TestParseLines_SubagentCrossBatchCompletion(t *testing.T) {
	// Simulate cross-batch completion: subagent created in batch 1, tool_result
	// arrives in batch 2 (via knownParents).
	batch1 := [][]byte{
		// progress entry: subagent tu-x with parentToolUseID tu-parent
		[]byte(`{"type":"progress","toolUseID":"tu-x","parentToolUseID":"tu-parent","sessionId":"s1","slug":"sub-slug","timestamp":"2026-01-01T10:00:01.000Z","data":{"type":"agent_progress","message":{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}}}}}`),
	}
	r1 := parseLines(batch1)

	sub1 := r1.subagents["tu-x"]
	if sub1 == nil {
		t.Fatal("expected subagent 'tu-x' in batch 1")
		return
	}
	if sub1.completed {
		t.Error("subagent should not be completed yet")
	}

	parents := buildParentMap(r1.subagents)

	// batch 2: tool_result for tu-parent (which is sub's parentToolUseID)
	batch2 := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-parent","content":[]}]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:02.000Z","gitBranch":"main"}`),
	}
	r2 := parseLinesWithParents(batch2, parents)

	sub2 := r2.subagents["tu-x"]
	if sub2 == nil {
		t.Fatal("expected cross-batch subagent 'tu-x' in batch 2")
		return
	}
	if !sub2.completed {
		t.Error("expected subagent to be completed via cross-batch tool_result")
	}
}

func TestParseLines_SubagentSlugCaptured(t *testing.T) {
	// Verify that the subagent's slug is captured from progress records.
	lines := fixtureLines(t, "subagent_session.jsonl")
	r := parseLines(lines)

	sub := r.subagents["tu-agent-1"]
	if sub == nil {
		t.Fatal("expected subagent 'tu-agent-1'")
		return
	}
	if sub.slug != "child-agent-slug" {
		t.Errorf("subagent slug: got %q, want %q", sub.slug, "child-agent-slug")
	}
}

func TestParse_SubagentSlugInSourceUpdate(t *testing.T) {
	// Verify the slug propagates through to the SourceUpdate's SubagentState.
	s := newTestSource(t)
	h := source.SessionHandle{
		ID:     "cccccccc-0000-0000-0000-000000000003",
		Path:   filepath.Join("testdata", "subagent_session.jsonl"),
		Source: "claude",
	}

	update, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(update.Subagents) != 1 {
		t.Fatalf("Subagents: got %d, want 1", len(update.Subagents))
	}
	if update.Subagents[0].Slug != "child-agent-slug" {
		t.Errorf("Subagent.Slug: got %q, want %q", update.Subagents[0].Slug, "child-agent-slug")
	}
}

func TestParseLines_ProgressSelfFilteredBySlug(t *testing.T) {
	// A non-agent progress entry whose slug matches the session slug should be
	// filtered out (it's the session's own progress, not a subagent's).
	lines := [][]byte{
		// Enrich assistant record so session slug is set.
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main","slug":"session-slug"}`),
		// Non-agent progress with same slug — should be filtered.
		[]byte(`{"type":"progress","toolUseID":"tu-1","slug":"session-slug","sessionId":"s1","timestamp":"2026-01-01T10:00:01.000Z","data":{"type":"tool_progress","message":{"type":"assistant","message":{}}}}`),
	}
	r := parseLines(lines)
	if len(r.subagents) != 0 {
		t.Errorf("expected 0 subagents (self-progress filtered), got %d", len(r.subagents))
	}
}

// --- Compaction tests ---

func TestParseLines_CompactionEvent(t *testing.T) {
	// A compaction record should increment compactionDelta and not affect other counts.
	lines := [][]byte{
		[]byte(`{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","summary":"compacted context"}`),
	}
	r := parseLines(lines)
	if r.compactionDelta != 1 {
		t.Errorf("compactionDelta: got %d, want 1", r.compactionDelta)
	}
	if r.msgDelta != 0 {
		t.Errorf("msgDelta: got %d, want 0 (compaction must not count as a message)", r.msgDelta)
	}
}

func TestParseLines_MultipleCompactionEvents(t *testing.T) {
	// Multiple compaction records in one batch should accumulate.
	lines := [][]byte{
		[]byte(`{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z"}`),
		[]byte(`{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:01:00.000Z"}`),
		[]byte(`{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:02:00.000Z"}`),
	}
	r := parseLines(lines)
	if r.compactionDelta != 3 {
		t.Errorf("compactionDelta: got %d, want 3", r.compactionDelta)
	}
}

func TestParseLines_CompactionWithOtherRecords(t *testing.T) {
	// Compaction records mixed with user/assistant records — only compactions
	// should increment compactionDelta, not affect message count logic.
	lines := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`),
		[]byte(`{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:00:01.000Z"}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:02.000Z","gitBranch":"main","slug":"test"}`),
	}
	r := parseLines(lines)
	if r.compactionDelta != 1 {
		t.Errorf("compactionDelta: got %d, want 1", r.compactionDelta)
	}
	if r.msgDelta != 2 {
		t.Errorf("msgDelta: got %d, want 2 (user + assistant)", r.msgDelta)
	}
}

func TestParse_CompactionCountDeltaInSourceUpdate(t *testing.T) {
	// Parse should propagate compactionDelta to SourceUpdate.CompactionCountDelta.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"compaction","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:01.000Z","gitBranch":"main"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := newTestSource(t)
	h := source.SessionHandle{ID: "s1", Path: path, Source: "claude"}
	update, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.CompactionCountDelta != 1 {
		t.Errorf("CompactionCountDelta: got %d, want 1", update.CompactionCountDelta)
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

// --- buildSubagentStates tests ---

func TestBuildSubagentStates_Empty(t *testing.T) {
	result := buildSubagentStates(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
	result = buildSubagentStates(map[string]*subagentResult{})
	if result != nil {
		t.Errorf("expected nil for empty map, got %v", result)
	}
}

func TestBuildSubagentStates_Fields(t *testing.T) {
	now := time.Now()
	subs := map[string]*subagentResult{
		"tu-1": {
			id:              "tu-1",
			parentToolUseID: "tu-parent",
			slug:            "explore-agent",
			activity:        session.ActivityWorking,
			currentTool:     "Read",
			firstTime:       now.Add(-time.Minute),
			lastTime:        now,
		},
	}
	states := buildSubagentStates(subs)
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	s := states[0]
	if s.ID != "tu-1" {
		t.Errorf("ID: got %q", s.ID)
	}
	if s.ParentID != "tu-parent" {
		t.Errorf("ParentID: got %q", s.ParentID)
	}
	if s.Slug != "explore-agent" {
		t.Errorf("Slug: got %q, want %q", s.Slug, "explore-agent")
	}
	if s.Activity != session.ActivityWorking {
		t.Errorf("Activity: got %q", s.Activity)
	}
	if s.CurrentTool != "Read" {
		t.Errorf("CurrentTool: got %q", s.CurrentTool)
	}
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

func TestParse_SubagentSession(t *testing.T) {
	s := newTestSource(t)
	h := source.SessionHandle{
		ID:     "cccccccc-0000-0000-0000-000000000003",
		Path:   filepath.Join("testdata", "subagent_session.jsonl"),
		Source: "claude",
	}

	update, _, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(update.Subagents) != 1 {
		t.Fatalf("Subagents: got %d, want 1", len(update.Subagents))
	}
	sub := update.Subagents[0]
	if sub.ID != "tu-agent-1" {
		t.Errorf("Subagent.ID: got %q", sub.ID)
	}
	if sub.Activity != session.ActivityTerminal {
		t.Errorf("Subagent.Activity: got %q, want %q", sub.Activity, session.ActivityTerminal)
	}
	if update.Terminal {
		t.Error("session should not be terminal (no end marker)")
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
		_ = f.Close()
		t.Fatalf("append line3: %v", err)
	}
	_ = f.Close()

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

// --- End marker tests ---

func writeEndMarker(t *testing.T, dir string, filename string, marker map[string]string) {
	t.Helper()
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestParse_EndMarker_TerminatesSession(t *testing.T) {
	// Write a JSONL session file and an end marker. Parse should return
	// Terminal=true with the correct EndReason.
	dir := t.TempDir()
	markerDir := t.TempDir()

	sessionID := "dddddddd-1111-2222-3333-000000000004"
	path := filepath.Join(dir, sessionID+".jsonl")
	line := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"bye"}]},"cwd":"/proj","sessionId":"` + sessionID + `","timestamp":"2026-01-01T13:00:00.000Z","gitBranch":"main"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	writeEndMarker(t, markerDir, "marker1.json", map[string]string{
		"session_id":      sessionID,
		"transcript_path": "/some/path/" + sessionID + ".jsonl",
		"cwd":             "/proj",
		"reason":          "exit_code",
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	})

	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: sessionID, Path: path, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !update.Terminal {
		t.Error("expected Terminal=true from end marker")
	}
	if update.EndReason != "exit_code" {
		t.Errorf("EndReason: got %q, want %q", update.EndReason, "exit_code")
	}
	if update.EndedAt.IsZero() {
		t.Error("EndedAt should be set from marker timestamp")
	}

	// Verify the marker file was consumed.
	entries, _ := os.ReadDir(markerDir)
	if len(entries) != 0 {
		t.Errorf("marker file should be deleted after consumption, found %d files", len(entries))
	}
}

func TestParse_EndMarker_NoData_StillTerminates(t *testing.T) {
	// An end marker should signal terminal even if the JSONL has no new data.
	dir := t.TempDir()
	markerDir := t.TempDir()

	sessionID := "eeeeeeee-1111-2222-3333-000000000005"
	// Empty JSONL file — no data to parse.
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	writeEndMarker(t, markerDir, "m.json", map[string]string{
		"session_id":      sessionID,
		"transcript_path": "/path/" + sessionID + ".jsonl",
		"reason":          "interrupt",
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	})

	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: sessionID, Path: path, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !update.Terminal {
		t.Error("expected Terminal=true even with empty JSONL")
	}
	if update.EndReason != "interrupt" {
		t.Errorf("EndReason: got %q, want %q", update.EndReason, "interrupt")
	}
}

func TestParse_EndMarker_MarkerNotForThisSession(t *testing.T) {
	// A marker for a different session should not affect this session.
	dir := t.TempDir()
	markerDir := t.TempDir()

	sessionID := "my-session-id"
	path := filepath.Join(dir, sessionID+".jsonl")
	line := `{"type":"user","message":{"role":"user","content":[]},"cwd":"/p","sessionId":"` + sessionID + `","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	writeEndMarker(t, markerDir, "other.json", map[string]string{
		"session_id":      "other-session-id",
		"transcript_path": "/path/other-session-id.jsonl",
		"reason":          "exit_code",
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	})

	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: sessionID, Path: path, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.Terminal {
		t.Error("session should not be terminal: marker is for a different session")
	}
}

// --- End marker validation tests ---

func TestValidateEndMarker_EmptySessionID(t *testing.T) {
	m := &sessionEndMarker{}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for empty session_id")
	}
}

func TestValidateEndMarker_SessionIDTooLong(t *testing.T) {
	m := &sessionEndMarker{SessionID: strings.Repeat("a", maxSessionIDLen+1)}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for too-long session_id")
	}
}

func TestValidateEndMarker_SessionIDInvalidChars(t *testing.T) {
	m := &sessionEndMarker{SessionID: "bad/session/../id"}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for session_id with invalid chars")
	}
}

func TestValidateEndMarker_TranscriptPathTraversal(t *testing.T) {
	m := &sessionEndMarker{
		SessionID:      "valid-id",
		TranscriptPath: "/home/user/../secret.jsonl",
	}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for path traversal in transcript_path")
	}
}

func TestValidateEndMarker_TranscriptPathNotJSONL(t *testing.T) {
	m := &sessionEndMarker{
		SessionID:      "valid-id",
		TranscriptPath: "/home/user/session.txt",
	}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for non-.jsonl transcript_path")
	}
}

func TestValidateEndMarker_TranscriptPathTooLong(t *testing.T) {
	m := &sessionEndMarker{
		SessionID:      "valid-id",
		TranscriptPath: "/" + strings.Repeat("a", maxTranscriptPathLen) + ".jsonl",
	}
	if err := validateEndMarker(m, time.Now()); err == nil {
		t.Error("expected error for too-long transcript_path")
	}
}

func TestValidateEndMarker_ReasonTruncated(t *testing.T) {
	m := &sessionEndMarker{
		SessionID: "valid-id",
		Reason:    strings.Repeat("x", maxReasonLen+100),
	}
	if err := validateEndMarker(m, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Reason) != maxReasonLen {
		t.Errorf("reason should be truncated to %d, got %d", maxReasonLen, len(m.Reason))
	}
}

func TestValidateEndMarker_TimestampSkew(t *testing.T) {
	// A timestamp more than 1 hour in the past should be cleared.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	m := &sessionEndMarker{
		SessionID: "valid-id",
		Timestamp: old,
	}
	if err := validateEndMarker(m, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Timestamp != "" {
		t.Errorf("out-of-skew timestamp should be cleared, got %q", m.Timestamp)
	}
}

func TestValidateEndMarker_TimestampInvalidFormat(t *testing.T) {
	m := &sessionEndMarker{
		SessionID: "valid-id",
		Timestamp: "not-a-timestamp",
	}
	if err := validateEndMarker(m, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Timestamp != "" {
		t.Errorf("invalid timestamp should be cleared, got %q", m.Timestamp)
	}
}

func TestParse_EndMarker_OversizeRejected(t *testing.T) {
	// A marker file larger than maxEndMarkerSize should be removed without
	// causing a terminal signal.
	dir := t.TempDir()
	markerDir := t.TempDir()

	sessionID := "ff-oversize-session"
	path := filepath.Join(dir, sessionID+".jsonl")
	line := `{"type":"user","message":{"role":"user","content":[]},"cwd":"/p","sessionId":"` + sessionID + `","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// Write a marker file bigger than the cap.
	bigMarkerPath := filepath.Join(markerDir, "big.json")
	if err := os.WriteFile(bigMarkerPath, []byte(strings.Repeat("x", defaultMaxEndMarkerSize+1)), 0o644); err != nil {
		t.Fatalf("write big marker: %v", err)
	}

	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: sessionID, Path: path, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.Terminal {
		t.Error("oversize marker must not produce Terminal=true")
	}

	// Oversize file should be deleted.
	entries, _ := os.ReadDir(markerDir)
	if len(entries) != 0 {
		t.Errorf("oversize marker file should be deleted, found %d files", len(entries))
	}
}

func TestWithMaxEndMarkerSize(t *testing.T) {
	dir := t.TempDir()
	markerDir := t.TempDir()

	sessionID := "gg-custom-size"
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	// Write a marker that is exactly 200 bytes (valid JSON).
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	markerData, _ := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": "/path/" + sessionID + ".jsonl",
		"reason":          "exit_code",
		"timestamp":       ts,
	})

	markerPath := filepath.Join(markerDir, "m.json")
	if err := os.WriteFile(markerPath, markerData, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Set max size to 50 bytes — smaller than the marker.
	src, err := New(WithSessionEndDir(markerDir), WithMaxEndMarkerSize(50))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: sessionID, Path: path, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.Terminal {
		t.Error("marker should be rejected when larger than WithMaxEndMarkerSize")
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

func TestDiscover_WorkingDirDecoded(t *testing.T) {
	// Sessions under a project directory named -tmp-myproject should have
	// WorkingDir set to the decoded path.
	dir := t.TempDir()
	proj := filepath.Join(dir, "-tmp-myproject")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "session1.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(WithRoot(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("got %d handles, want 1", len(handles))
	}
	// The decoded WorkingDir should be the fallback /tmp/myproject
	// (filesystem verification won't match, so we get the dash-to-slash fallback).
	if handles[0].WorkingDir == "" {
		t.Error("WorkingDir should be non-empty")
	}
	if handles[0].WorkingDir == "-tmp-myproject" {
		t.Error("WorkingDir should be decoded, not raw encoded form")
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

// --- Project path encoding/decoding tests ---

func TestEncodeProjectPath(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/home/user/project", "-home-user-project"},
		{"/tmp/myproject", "-tmp-myproject"},
		{"/", "-"},
		{"/a/b/c", "-a-b-c"},
	}
	for _, tc := range cases {
		got := encodeProjectPath(tc.input)
		if got != tc.want {
			t.Errorf("encodeProjectPath(%q): got %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDecodeProjectPath_FallbackNoDashes(t *testing.T) {
	// A path that doesn't start with '-' is returned as-is.
	got := decodeProjectPath("home-user-project")
	if got != "home-user-project" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeProjectPath_FallbackAllDashes(t *testing.T) {
	// When no filesystem path can be verified, fall back to treating all
	// dashes as slashes.
	got := decodeProjectPath("-nonexistent-path-deep")
	if got != "/nonexistent/path/deep" {
		t.Errorf("got %q, want %q", got, "/nonexistent/path/deep")
	}
}

func TestDecodeProjectPath_RoundTrip(t *testing.T) {
	// On a path that actually exists on the filesystem, encode->decode should
	// round-trip to the original path (the filesystem-verified result).
	original := t.TempDir() // guaranteed to exist
	encoded := encodeProjectPath(original)
	decoded := decodeProjectPath(encoded)
	if decoded != original {
		t.Errorf("round-trip: encode(%q) = %q, decode = %q", original, encoded, decoded)
	}
}

// --- Cursor encoding/decoding tests ---

func TestEncodeDecode_PlainOffset(t *testing.T) {
	cc := claudeCursor{Offset: 12345}
	c, err := encodeCursor(cc)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	// Without parents, should encode as plain integer.
	if string(c) != "12345" {
		t.Errorf("expected plain integer cursor, got %q", c)
	}

	cc2, err := decodeCursor(c)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if cc2.Offset != 12345 {
		t.Errorf("offset: got %d, want 12345", cc2.Offset)
	}
}

func TestEncodeDecode_WithParents(t *testing.T) {
	cc := claudeCursor{
		Offset:  99,
		Parents: map[string]string{"parent-id": "sub-id"},
	}
	c, err := encodeCursor(cc)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	// With parents, should be JSON.
	cc2, err := decodeCursor(c)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if cc2.Offset != 99 {
		t.Errorf("offset: got %d, want 99", cc2.Offset)
	}
	if cc2.Parents["parent-id"] != "sub-id" {
		t.Errorf("parents: got %v", cc2.Parents)
	}
}

func TestDecodeCursor_Empty(t *testing.T) {
	cc, err := decodeCursor("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.Offset != 0 {
		t.Errorf("empty cursor should have offset 0, got %d", cc.Offset)
	}
}

func TestDecodeCursor_Corrupt(t *testing.T) {
	// Corrupt cursor should reset to beginning in Parse (handled at caller).
	cc, err := decodeCursor("not-a-number-or-json")
	if err == nil {
		t.Error("expected error for corrupt cursor")
	}
	_ = cc
}

