package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

const (
	layoutParentID = "ffffffff-0000-0000-0000-000000000010"
	layoutAgent1   = "a0000000000000001"
	layoutAgent2   = "a0000000000000002"
)

// copyLayoutFixture copies testdata/subagent_layout into a temp dir so tests
// can append to the files, and returns the root plus the parent's path.
func copyLayoutFixture(t *testing.T) (root, parentPath string) {
	t.Helper()
	src := filepath.Join("testdata", "subagent_layout")
	root = t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return root, filepath.Join(root, "-tmp-proj", layoutParentID+".jsonl")
}

func layoutAgentPath(root, agentID string) string {
	return filepath.Join(root, "-tmp-proj", layoutParentID, "subagents", "agent-"+agentID+".jsonl")
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func subagentByID(subs []session.SubagentState, id string) (session.SubagentState, bool) {
	for i := 0; i < len(subs); i++ {
		if subs[i].ID == id {
			return subs[i], true
		}
	}
	return session.SubagentState{}, false
}

func TestDiscover_SubagentLayout_YieldsOnlyParent(t *testing.T) {
	root, _ := copyLayoutFixture(t)
	src, err := New(WithRoot(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 1 {
		ids := make([]string, 0, len(handles))
		for i := 0; i < len(handles); i++ {
			ids = append(ids, handles[i].ID)
		}
		t.Fatalf("handles: got %d %v, want 1 (subagent files must not be top-level sessions)", len(handles), ids)
	}
	if handles[0].ID != layoutParentID {
		t.Errorf("handle ID: got %q, want %q", handles[0].ID, layoutParentID)
	}
}

func TestParse_SubagentLayout_AttachesSubagentsToParent(t *testing.T) {
	_, parentPath := copyLayoutFixture(t)
	src, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: layoutParentID, Path: parentPath, Source: "claude"}

	update, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if update.MessageCountDelta != 3 {
		t.Errorf("parent MessageCountDelta: got %d, want 3 (subagent messages must not count)", update.MessageCountDelta)
	}
	if update.Model != "claude-opus-4-6" {
		t.Errorf("parent Model: got %q", update.Model)
	}
	if len(update.Subagents) != 2 {
		t.Fatalf("Subagents: got %d, want 2", len(update.Subagents))
	}

	a1, ok := subagentByID(update.Subagents, layoutAgent1)
	if !ok {
		t.Fatalf("subagent %s missing", layoutAgent1)
	}
	if a1.ParentID != "toolu_sub_1" {
		t.Errorf("a1 ParentID: got %q, want toolu_sub_1", a1.ParentID)
	}
	if a1.Model != "claude-sonnet-4-6" {
		t.Errorf("a1 Model: got %q", a1.Model)
	}
	if a1.MessageCount != 4 || a1.ToolCallCount != 1 {
		t.Errorf("a1 counts: got msg=%d tools=%d, want 4/1", a1.MessageCount, a1.ToolCallCount)
	}
	if a1.ContextTokens != 50 || a1.OutputTokens != 6 {
		t.Errorf("a1 tokens: got ctx=%d out=%d, want 50/6", a1.ContextTokens, a1.OutputTokens)
	}
	if a1.Activity != session.ActivityTerminal || a1.CompletedAt.IsZero() {
		t.Errorf("a1 should be completed by the parent's tool_result: activity=%q completedAt=%v", a1.Activity, a1.CompletedAt)
	}
	if a1.Slug != "" {
		t.Errorf("a1 Slug: got %q, want empty", a1.Slug)
	}
	wantStart := time.Date(2026, 11, 1, 10, 0, 2, 0, time.UTC)
	if !a1.StartedAt.Equal(wantStart) {
		t.Errorf("a1 StartedAt: got %v, want %v", a1.StartedAt, wantStart)
	}

	a2, ok := subagentByID(update.Subagents, layoutAgent2)
	if !ok {
		t.Fatalf("subagent %s missing", layoutAgent2)
	}
	if a2.Activity != session.ActivityWorking {
		t.Errorf("a2 Activity: got %q, want working", a2.Activity)
	}
	if a2.CurrentTool != "Grep" {
		t.Errorf("a2 CurrentTool: got %q, want Grep", a2.CurrentTool)
	}
	if a2.ParentID != "toolu_sub_2" {
		t.Errorf("a2 ParentID: got %q, want toolu_sub_2", a2.ParentID)
	}
}

func TestParse_SubagentFile_DoesNotConsumeParentEndMarker(t *testing.T) {
	root, parentPath := copyLayoutFixture(t)
	markerDir := t.TempDir()
	writeEndMarker(t, markerDir, layoutParentID+".json", map[string]string{
		"session_id": layoutParentID,
		"reason":     "exit",
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	})
	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A handle for a subagent file whose records carry the parent's sessionId.
	subH := source.SessionHandle{ID: "agent-" + layoutAgent1, Path: layoutAgentPath(root, layoutAgent1), Source: "claude"}
	subUpdate, _, err := src.Parse(context.Background(), subH, "")
	if err != nil {
		t.Fatalf("Parse subagent: %v", err)
	}
	if subUpdate.Terminal {
		t.Error("subagent file consumed the parent's end marker")
	}

	parentH := source.SessionHandle{ID: layoutParentID, Path: parentPath, Source: "claude"}
	parentUpdate, _, err := src.Parse(context.Background(), parentH, "")
	if err != nil {
		t.Fatalf("Parse parent: %v", err)
	}
	if !parentUpdate.Terminal {
		t.Error("parent should go terminal from its own end marker")
	}
}

func TestParse_SubagentLayout_OnlySubagentChanged(t *testing.T) {
	root, parentPath := copyLayoutFixture(t)
	src, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: layoutParentID, Path: parentPath, Source: "claude"}

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse 1: %v", err)
	}

	// No change anywhere: zero update.
	idle, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse 2: %v", err)
	}
	if idle.SessionID != "" {
		t.Errorf("expected zero update with no new data, got SessionID=%q subs=%d", idle.SessionID, len(idle.Subagents))
	}

	appendLine(t, layoutAgentPath(root, layoutAgent2),
		`{"type":"assistant","isSidechain":true,"agentId":"a0000000000000002","message":{"role":"assistant","model":"claude-haiku-4-5","content":[{"type":"tool_use","id":"toolu_a2_2","name":"Read"}],"stop_reason":"tool_use","usage":{"input_tokens":35,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":2}},"cwd":"/tmp/proj","sessionId":"ffffffff-0000-0000-0000-000000000010","timestamp":"2026-11-01T10:00:40.000Z","gitBranch":"main"}`)

	update, _, err := src.Parse(context.Background(), h, c2)
	if err != nil {
		t.Fatalf("Parse 3: %v", err)
	}
	if update.SessionID != layoutParentID {
		t.Fatalf("subagent-only change must still report the parent session, got SessionID=%q", update.SessionID)
	}
	if update.MessageCountDelta != 0 {
		t.Errorf("parent MessageCountDelta: got %d, want 0", update.MessageCountDelta)
	}
	// Re-reports the parent's last tokens so the monitor does not zero them.
	if update.ContextTokens != 100 || update.OutputTokens != 5 {
		t.Errorf("parent tokens: got ctx=%d out=%d, want 100/5", update.ContextTokens, update.OutputTokens)
	}
	a2, ok := subagentByID(update.Subagents, layoutAgent2)
	if !ok {
		t.Fatalf("subagent %s missing from update", layoutAgent2)
	}
	if a2.MessageCount != 3 || a2.ToolCallCount != 2 {
		t.Errorf("a2 counts must accumulate across polls: got msg=%d tools=%d, want 3/2", a2.MessageCount, a2.ToolCallCount)
	}
	if a2.CurrentTool != "Read" {
		t.Errorf("a2 CurrentTool: got %q, want Read", a2.CurrentTool)
	}
}

func TestMonitor_SubagentLayout_PersistsAcrossBatches(t *testing.T) {
	root, _ := copyLayoutFixture(t)
	src, err := New(WithRoot(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	appendLine(t, layoutAgentPath(root, layoutAgent2),
		`{"type":"user","isSidechain":true,"agentId":"a0000000000000002","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a2_1","content":[]}]},"cwd":"/tmp/proj","sessionId":"ffffffff-0000-0000-0000-000000000010","timestamp":"2026-11-01T10:00:41.000Z","gitBranch":"main"}`)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("sessions: got %d, want 1", len(snap))
	}
	subs := snap[0].Subagents
	if len(subs) != 2 {
		t.Fatalf("subagents after batch 2: got %d, want 2", len(subs))
	}
	a1, ok := subagentByID(subs, layoutAgent1)
	if !ok {
		t.Fatal("batch-1 subagent vanished after batch 2")
	}
	if a1.Model != "claude-sonnet-4-6" || a1.MessageCount != 4 || a1.Activity != session.ActivityTerminal {
		t.Errorf("a1 lost state: %+v", a1)
	}
	a2, _ := subagentByID(subs, layoutAgent2)
	if a2.MessageCount != 3 {
		t.Errorf("a2 MessageCount: got %d, want 3", a2.MessageCount)
	}
	if snap[0].ContextTokens != 100 {
		t.Errorf("parent ContextTokens zeroed by subagent-only update: got %d", snap[0].ContextTokens)
	}
}

func TestParse_LegacyProgress_SubagentAccumulatesAcrossBatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	progress := func(ts string) string {
		return `{"type":"progress","toolUseID":"tu-x","parentToolUseID":"tu-parent","sessionId":"s1","slug":"sub-slug","timestamp":"` + ts + `","data":{"type":"agent_progress","message":{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","name":"Read"}],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}}}}}`
	}
	userLine := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"sanitized"}]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`
	if err := os.WriteFile(path, []byte(userLine+"\n"+progress("2026-01-01T10:00:01.000Z")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: "s1", Path: path, Source: "claude"}
	ctx := context.Background()

	_, c1, err := src.Parse(ctx, h, "")
	if err != nil {
		t.Fatalf("Parse 1: %v", err)
	}

	appendLine(t, path, progress("2026-01-01T10:00:02.000Z"))
	u2, c2, err := src.Parse(ctx, h, c1)
	if err != nil {
		t.Fatalf("Parse 2: %v", err)
	}
	sub, ok := subagentByID(u2.Subagents, "tu-x")
	if !ok {
		t.Fatal("progress-only batch dropped the subagent update")
	}
	if sub.MessageCount != 2 || sub.ToolCallCount != 2 {
		t.Errorf("counts reset across batches: got msg=%d tools=%d, want 2/2", sub.MessageCount, sub.ToolCallCount)
	}
	wantStart := time.Date(2026, 1, 1, 10, 0, 1, 0, time.UTC)
	if !sub.StartedAt.Equal(wantStart) {
		t.Errorf("StartedAt: got %v, want %v", sub.StartedAt, wantStart)
	}

	appendLine(t, path, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-parent","content":[]}]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:03.000Z","gitBranch":"main"}`)
	u3, _, err := src.Parse(ctx, h, c2)
	if err != nil {
		t.Fatalf("Parse 3: %v", err)
	}
	done, ok := subagentByID(u3.Subagents, "tu-x")
	if !ok {
		t.Fatal("cross-batch completion missing")
	}
	if done.Activity != session.ActivityTerminal {
		t.Errorf("Activity: got %q, want terminal", done.Activity)
	}
	if done.Slug != "sub-slug" || done.Model != "claude-sonnet-4-6" || done.MessageCount != 2 {
		t.Errorf("cross-batch completion replaced full entry with a stub: %+v", done)
	}
}
