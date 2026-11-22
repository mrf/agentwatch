package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Shapes below mirror real Claude Code transcripts (keys and type/subtype
// values only; all text is synthetic).
const (
	compactBoundaryLine = `{"parentUuid":null,"logicalParentUuid":"u0","isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","level":"info","compactMetadata":{"trigger":"manual","preTokens":177901,"postTokens":16589},"uuid":"u1","timestamp":"2026-01-01T10:00:00.000Z","cwd":"/p","sessionId":"s1","gitBranch":"main","slug":"a-slug"}`
	compactSummaryLine  = `{"parentUuid":"u1","isSidechain":false,"type":"user","message":{"role":"user","content":"summary text"},"isVisibleInTranscriptOnly":true,"isCompactSummary":true,"uuid":"u2","timestamp":"2026-01-01T10:00:00.100Z","cwd":"/p","sessionId":"s1","gitBranch":"main","slug":"a-slug"}`
)

func TestParseLines_CompactBoundaryCountsOnce(t *testing.T) {
	r := parseLines([][]byte{[]byte(compactBoundaryLine), []byte(compactSummaryLine)})
	if r.compactionDelta != 1 {
		t.Errorf("compactionDelta: got %d, want 1 (boundary counts, summary does not)", r.compactionDelta)
	}
}

func TestParseLines_CompactSummaryIsNotAMessage(t *testing.T) {
	r := parseLines([][]byte{[]byte(compactBoundaryLine), []byte(compactSummaryLine)})
	if r.msgDelta != 0 {
		t.Errorf("msgDelta: got %d, want 0 (synthetic summary is not a user turn)", r.msgDelta)
	}
	if r.hasData {
		t.Error("hasData: compaction markers alone must not report conversation data")
	}
}

func TestParseLines_UnknownSystemSubtypesIgnored(t *testing.T) {
	// Shapes mirror real transcripts (keys and subtype values only).
	subtypes := []string{"turn_duration", "scheduled_task_fire", "local_command", "informational", "api_error", "stop_hook_summary", "some_future_subtype"}
	for i := 0; i < len(subtypes); i++ {
		subtype := subtypes[i]
		t.Run(subtype, func(t *testing.T) {
			line := `{"type":"system","subtype":"` + subtype + `","content":"x","level":"info","durationMs":1200,"isSidechain":false,"timestamp":"2026-01-01T10:00:00.000Z","sessionId":"s1","cwd":"/p"}`
			r := parseLines([][]byte{[]byte(line)})
			if r.compactionDelta != 0 || r.msgDelta != 0 || r.toolDelta != 0 {
				t.Errorf("counts changed: compaction=%d msg=%d tool=%d", r.compactionDelta, r.msgDelta, r.toolDelta)
			}
			if r.activity != session.ActivityIdle {
				t.Errorf("activity: got %q, want idle (default)", r.activity)
			}
			if r.hasData {
				t.Error("hasData: system subtype must not report conversation data")
			}
		})
	}
}

func TestParse_CompactBoundaryPropagatesToSourceUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	content := compactBoundaryLine + "\n" + compactSummaryLine + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:01.000Z"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := newTestSource(t)
	update, _, err := s.Parse(context.Background(), source.SessionHandle{ID: "s1", Path: path, Source: "claude"}, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.CompactionCountDelta != 1 {
		t.Errorf("CompactionCountDelta: got %d, want 1", update.CompactionCountDelta)
	}
	if update.MessageCountDelta != 1 {
		t.Errorf("MessageCountDelta: got %d, want 1", update.MessageCountDelta)
	}
}

func TestParse_CompactionOnlyBatchStillReportsDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(path, []byte(compactBoundaryLine+"\n"+compactSummaryLine+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := newTestSource(t)
	update, _, err := s.Parse(context.Background(), source.SessionHandle{ID: "s1", Path: path, Source: "claude"}, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.CompactionCountDelta != 1 {
		t.Errorf("CompactionCountDelta: got %d, want 1", update.CompactionCountDelta)
	}
	if update.Activity != "" {
		t.Errorf("Activity: got %q, want empty (a marker is not activity evidence)", update.Activity)
	}
}

func TestParseLines_SyntheticModelDoesNotOverwrite(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","isSidechain":false,"cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","message":{"role":"assistant","model":"claude-opus-4-1","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10},"content":[]}}`),
		[]byte(`{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"error":"authentication_failed","cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:01.000Z","message":{"role":"assistant","model":"<synthetic>","stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0},"content":[]}}`),
	}
	r := parseLines(lines)
	if r.model != "claude-opus-4-1" {
		t.Errorf("model: got %q, want claude-opus-4-1", r.model)
	}
	if r.contextTokens != 100 || r.outputTokens != 10 {
		t.Errorf("tokens: got ctx=%d out=%d, want 100/10 (synthetic zero usage must not reset)", r.contextTokens, r.outputTokens)
	}
	if r.msgDelta != 2 {
		t.Errorf("msgDelta: got %d, want 2 (synthetic turn is still a message)", r.msgDelta)
	}
}

func TestParseLines_SyntheticModelAloneLeavesModelEmpty(t *testing.T) {
	line := `{"type":"assistant","cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","message":{"role":"assistant","model":"<synthetic>","stop_reason":"stop_sequence","content":[]}}`
	r := parseLines([][]byte{[]byte(line)})
	if r.model != "" {
		t.Errorf("model: got %q, want empty", r.model)
	}
}

func TestParseLines_SidechainRecordsDoNotTouchParentState(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"hi"}]},"cwd":"/parent","gitBranch":"main","sessionId":"s1","slug":"parent-slug","timestamp":"2026-01-01T10:00:00.000Z"}`),
		[]byte(`{"type":"assistant","isSidechain":true,"cwd":"/side","gitBranch":"side","sessionId":"s1","slug":"side-slug","timestamp":"2026-01-01T10:00:05.000Z","message":{"role":"assistant","model":"side-model","stop_reason":"tool_use","usage":{"input_tokens":999,"output_tokens":9},"content":[{"type":"tool_use","name":"SideTool"}]}}`),
		[]byte(`{"type":"user","isSidechain":true,"message":{"role":"user","content":[{"type":"text","text":"x"}]},"cwd":"/side","sessionId":"s1","timestamp":"2026-01-01T10:00:06.000Z"}`),
	}
	r := parseLines(lines)
	if r.cwd != "/parent" || r.branch != "main" {
		t.Errorf("cwd/branch: got %q/%q, want /parent/main", r.cwd, r.branch)
	}
	if r.model != "" || r.hasTokens || r.currentTool != "" {
		t.Errorf("sidechain leaked: model=%q hasTokens=%v tool=%q", r.model, r.hasTokens, r.currentTool)
	}
	if r.msgDelta != 1 || r.toolDelta != 0 {
		t.Errorf("counts: msg=%d tool=%d, want 1/0", r.msgDelta, r.toolDelta)
	}
	if !r.lastActivityAt.Equal(mustParseTime(t, "2026-01-01T10:00:00.000Z")) {
		t.Errorf("lastActivityAt: got %v, sidechain timestamp must not advance it", r.lastActivityAt)
	}
}

func TestParseSubagentFile_SidechainRecordsStillCounted(t *testing.T) {
	// A subagent's own transcript is all isSidechain:true; it is that
	// subagent's state, not parent state.
	lines := [][]byte{
		[]byte(`{"type":"assistant","isSidechain":true,"agentId":"a1","cwd":"/p","sessionId":"s1","timestamp":"2026-01-01T10:00:05.000Z","message":{"role":"assistant","model":"sub-model","stop_reason":"tool_use","usage":{"input_tokens":50,"output_tokens":5},"content":[{"type":"tool_use","name":"Read"}]}}`),
	}
	r := parseSubagentLines(lines)
	if r.model != "sub-model" || r.msgDelta != 1 || r.toolDelta != 1 {
		t.Errorf("subagent file: model=%q msg=%d tool=%d, want sub-model/1/1", r.model, r.msgDelta, r.toolDelta)
	}
}

func TestDecodeProjectPath_DotEncodedComponent(t *testing.T) {
	// Claude Code encodes '.' as '-' too: /x/proj/.claude/wt → -x-proj--claude-wt.
	root := t.TempDir()
	dotDir := filepath.Join(root, "proj", ".claude", "wt")
	if err := os.MkdirAll(dotDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := decodeProjectPath(encodeProjectPath(dotDir))
	if got != dotDir {
		t.Errorf("decode(encode(%q)) = %q", dotDir, got)
	}
}
