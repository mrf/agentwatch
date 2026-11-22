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

const (
	testID  = "c3558800-3159-492f-bfc7-edc483c1477f"
	testID2 = "18fa5a17-5f09-4458-850b-bde7ad87cc2c"
)

func newTestSource(t *testing.T, opts ...Option) *AntigravitySource {
	t.Helper()
	s, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.(*AntigravitySource)
}

func parse(t *testing.T, s *AntigravitySource, c *convDB, cur source.Cursor) (source.SourceUpdate, source.Cursor) {
	t.Helper()
	h := source.SessionHandle{ID: testID, Path: c.path, Source: "antigravity"}
	u, next, err := s.Parse(context.Background(), h, cur)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return u, next
}

// --- Parse ---

func TestParse_FreshConversation(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.setMeta("file:///home/u/my%20proj", fixtureStart)
	c.addExecutor(0, "gemini-4-neon-xhigh", 262144)
	c.addStep(stepUserInput, statusDone, stepMeta(at(0)))

	u, cur := parse(t, newTestSource(t), c, "")
	if cur == "" {
		t.Error("cursor not advanced")
	}
	if u.SessionID != testID {
		t.Errorf("SessionID = %q", u.SessionID)
	}
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %v, want working (prompt submitted)", u.Activity)
	}
	if u.Model != "gemini-4-neon-xhigh" {
		t.Errorf("Model = %q", u.Model)
	}
	// executor field 10.1.6 (262144) is not the window: real prompts exceed
	// it without compaction, so no window is reported.
	if u.MaxContextTokens != 0 {
		t.Errorf("MaxContextTokens = %d, want 0 (unknown)", u.MaxContextTokens)
	}
	if u.MessageCountDelta != 1 || u.ToolCallCountDelta != 0 {
		t.Errorf("deltas = %d msgs, %d tools; want 1, 0", u.MessageCountDelta, u.ToolCallCountDelta)
	}
	if u.WorkingDir != "/home/u/my proj" {
		t.Errorf("WorkingDir = %q", u.WorkingDir)
	}
	if !u.StartedAt.Equal(fixtureStart) {
		t.Errorf("StartedAt = %v, want %v", u.StartedAt, fixtureStart)
	}
	if !u.LastActivityAt.Equal(at(0)) {
		t.Errorf("LastActivityAt = %v, want %v", u.LastActivityAt, at(0))
	}
	if u.HasTokens {
		t.Error("HasTokens = true before any planner response")
	}
}

func TestParse_ToolSession(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.setMeta("", fixtureStart)
	c.addExecutor(0, "gemini-4-neon-xhigh", 262144)
	c.addStep(stepUserInput, statusDone, stepMeta(at(0)))
	c.addStep(stepPlannerResponse, statusDone, plannerMeta(at(3), 16513, 456, 0))
	c.addStep(stepToolCall, statusDone, toolMeta(at(4), "run_command"))
	c.addStep(stepEphemeral, statusDone, stepMeta(at(5)))
	c.addStep(stepPlannerResponse, statusDone, plannerMeta(at(9), 2933, 703, 14791))
	c.addStep(stepToolCall, 2, toolMeta(at(10), "read_file"))

	u, _ := parse(t, newTestSource(t), c, "")
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %v, want working (tool running)", u.Activity)
	}
	if u.CurrentTool != "read_file" {
		t.Errorf("CurrentTool = %q, want read_file", u.CurrentTool)
	}
	if u.MessageCountDelta != 3 {
		t.Errorf("MessageCountDelta = %d, want 3 (1 user + 2 planner)", u.MessageCountDelta)
	}
	if u.ToolCallCountDelta != 2 {
		t.Errorf("ToolCallCountDelta = %d, want 2", u.ToolCallCountDelta)
	}
	if !u.HasTokens || u.ContextTokens != 2933+14791 || u.OutputTokens != 703 {
		t.Errorf("tokens = has %v ctx %d out %d; want true %d 703", u.HasTokens, u.ContextTokens, u.OutputTokens, 2933+14791)
	}
	if u.WorkingDir != "" {
		t.Errorf("WorkingDir = %q, want empty without workspace URI", u.WorkingDir)
	}
	if !u.LastActivityAt.Equal(at(10)) {
		t.Errorf("LastActivityAt = %v, want %v", u.LastActivityAt, at(10))
	}
}

func TestParse_Activity(t *testing.T) {
	type step struct{ typ, status int }
	tests := []struct {
		name  string
		steps []step
		want  session.Activity
	}{
		{"empty", nil, session.ActivityIdle},
		{"finished tool awaits planner", []step{{stepUserInput, 3}, {stepToolCall, 3}}, session.ActivityWorking},
		{"planner generating", []step{{stepUserInput, 3}, {stepPlannerResponse, 8}}, session.ActivityWorking},
		{"planner done", []step{{stepUserInput, 3}, {stepPlannerResponse, 3}}, session.ActivityWaiting},
		{"trailing ephemeral ignored", []step{{stepUserInput, 3}, {stepPlannerResponse, 3}, {stepEphemeral, 3}}, session.ActivityWaiting},
		// Observed live: a backgrounded command stays running after the
		// planner has finished its turn.
		{"dangling running tool before final planner", []step{{stepToolCall, 2}, {stepPlannerResponse, 3}}, session.ActivityWaiting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newConvDB(t, t.TempDir(), testID)
			for i := 0; i < len(tt.steps); i++ {
				c.addStep(tt.steps[i].typ, tt.steps[i].status, toolMeta(at(i), "run_command"))
			}
			u, _ := parse(t, newTestSource(t), c, "")
			if u.Activity != tt.want {
				t.Errorf("Activity = %v, want %v", u.Activity, tt.want)
			}
		})
	}
}

func TestParse_IncrementalDeltas(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.addStep(stepUserInput, statusDone, stepMeta(at(0)))
	s := newTestSource(t)

	_, cur := parse(t, s, c, "")

	u, same := parse(t, s, c, cur)
	if same != cur {
		t.Errorf("unchanged DB moved cursor %q -> %q", cur, same)
	}
	if u.MessageCountDelta != 0 || u.Activity != "" {
		t.Errorf("unchanged DB returned data: %+v", u)
	}

	planner := c.addStep(stepPlannerResponse, 8, plannerMeta(at(2), 100, 10, 0))
	c.addStep(stepToolCall, 1, toolMeta(at(3), "edit_file"))
	u, cur2 := parse(t, s, c, cur)
	if cur2 == cur {
		t.Fatal("cursor unchanged after new steps")
	}
	if u.MessageCountDelta != 1 || u.ToolCallCountDelta != 1 {
		t.Errorf("deltas = %d, %d; want 1, 1", u.MessageCountDelta, u.ToolCallCountDelta)
	}

	// An in-place status change is new data with zero count deltas.
	c.setStatus(planner, statusDone)
	u, cur3 := parse(t, s, c, cur2)
	if cur3 == cur2 {
		t.Fatal("cursor unchanged after status update")
	}
	if u.MessageCountDelta != 0 || u.ToolCallCountDelta != 0 {
		t.Errorf("status change deltas = %d, %d; want 0, 0", u.MessageCountDelta, u.ToolCallCountDelta)
	}
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %v, want working (last step is a tool call)", u.Activity)
	}
}

func TestParse_ReadsAfterCheckpointAndClose(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.addStep(stepUserInput, statusDone, stepMeta(at(0)))
	c.addStep(stepPlannerResponse, statusDone, plannerMeta(at(1), 10, 5, 0))
	c.exec("PRAGMA wal_checkpoint(TRUNCATE)")
	if err := c.db.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	u, _ := parse(t, newTestSource(t), c, "")
	if u.MessageCountDelta != 2 || u.Activity != session.ActivityWaiting {
		t.Errorf("got %d msgs, %v; want 2, waiting", u.MessageCountDelta, u.Activity)
	}
}

func TestParse_PlannerWithoutUsageHasNoTokens(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.addStep(stepPlannerResponse, statusDone, stepMeta(at(0)))
	u, _ := parse(t, newTestSource(t), c, "")
	if u.HasTokens {
		t.Error("HasTokens = true without usage metadata")
	}
}

func TestParse_WorkingDir(t *testing.T) {
	tests := []struct{ uri, handle, want string }{
		{"file:///home/u/proj", "/fallback", "/home/u/proj"},
		{"https://example.com/x", "/fallback", "/fallback"},
		{"", "/fallback", "/fallback"},
		{"", "", ""},
	}
	for _, tt := range tests {
		c := newConvDB(t, t.TempDir(), testID)
		c.setMeta(tt.uri, fixtureStart)
		h := source.SessionHandle{ID: testID, Path: c.path, WorkingDir: tt.handle}
		u, _, err := newTestSource(t).Parse(context.Background(), h, "")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if u.WorkingDir != tt.want {
			t.Errorf("uri %q handle %q: WorkingDir = %q, want %q", tt.uri, tt.handle, u.WorkingDir, tt.want)
		}
	}
}

func TestParse_CorruptFileAdvancesCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), testID+".db")
	if err := os.WriteFile(path, []byte("{not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestSource(t)
	h := source.SessionHandle{ID: testID, Path: path}
	_, cur, err := s.Parse(context.Background(), h, "")
	if err == nil {
		t.Fatal("expected error for corrupt DB")
	}
	if cur == "" {
		t.Fatal("cursor not advanced past corrupt DB")
	}
	if _, _, err := s.Parse(context.Background(), h, cur); err != nil {
		t.Errorf("unchanged corrupt DB re-parsed: %v", err)
	}
}

func TestParse_MalformedCursorReparses(t *testing.T) {
	c := newConvDB(t, t.TempDir(), testID)
	c.addStep(stepUserInput, statusDone, stepMeta(at(0)))
	u, _ := parse(t, newTestSource(t), c, "garbage,1")
	if u.MessageCountDelta != 1 {
		t.Errorf("MessageCountDelta = %d, want 1", u.MessageCountDelta)
	}
}

func TestParse_Errors(t *testing.T) {
	s := newTestSource(t)
	missing := source.SessionHandle{ID: testID, Path: filepath.Join(t.TempDir(), testID+".db")}
	if _, _, err := s.Parse(context.Background(), missing, ""); err == nil {
		t.Error("missing file: expected error")
	}

	c := newConvDB(t, t.TempDir(), testID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := source.SessionHandle{ID: testID, Path: c.path}
	if _, _, err := s.Parse(ctx, h, ""); err == nil {
		t.Error("cancelled context: expected error")
	}
}

// --- Discover ---

func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover_FindsConversationDBs(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	touch(t, filepath.Join(root, testID+".db"), now)
	touch(t, filepath.Join(root, testID+".db-wal"), now)
	touch(t, filepath.Join(root, testID+".db-shm"), now)
	touch(t, filepath.Join(root, testID2+".db"), now)
	touch(t, filepath.Join(root, "conversation_summaries.db"), now)
	touch(t, filepath.Join(root, testID+".json"), now)

	handles, err := newTestSource(t, WithRoot(root)).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("got %d handles, want 2: %+v", len(handles), handles)
	}
	for i := 0; i < len(handles); i++ {
		h := handles[i]
		if h.ID != testID && h.ID != testID2 {
			t.Errorf("unexpected ID %q", h.ID)
		}
		if h.Path != filepath.Join(root, h.ID+".db") {
			t.Errorf("Path = %q", h.Path)
		}
		if h.Source != "antigravity" || h.StartedAt.IsZero() {
			t.Errorf("handle = %+v", h)
		}
	}
}

func TestDiscover_WindowCountsWALActivity(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	// db mtime lags while agy's writes sit uncheckpointed in the WAL.
	touch(t, filepath.Join(root, testID+".db"), old)
	touch(t, filepath.Join(root, testID+".db-wal"), now)
	touch(t, filepath.Join(root, testID2+".db"), old)

	s := newTestSource(t, WithRoot(root), WithDiscoverWindow(time.Hour))
	s.now = func() time.Time { return now }
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 1 || handles[0].ID != testID {
		t.Fatalf("handles = %+v, want only %s", handles, testID)
	}
}

func TestDiscover_EmptyAndMissingRoot(t *testing.T) {
	for _, s := range []*AntigravitySource{
		newTestSource(t),
		newTestSource(t, WithRoot("/nonexistent/antigravity/path")),
	} {
		handles, err := s.Discover(context.Background())
		if err != nil || len(handles) != 0 {
			t.Errorf("root %q: handles %v err %v; want none, nil", s.root, handles, err)
		}
	}
}

func TestDiscover_ContextCancelled(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, testID+".db"), time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestSource(t, WithRoot(root)).Discover(ctx); err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Get("antigravity"); !ok {
		t.Error("antigravity not registered")
	}
	if err := Register(r); err == nil {
		t.Error("expected error on double Register")
	}
}
