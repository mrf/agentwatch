package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// --- test helpers ---

// recordingSink collects all events delivered to it.
type recordingSink struct {
	events []monitor.Event
}

func (s *recordingSink) HandleEvent(_ context.Context, ev monitor.Event) error {
	s.events = append(s.events, ev)
	return nil
}

// funcSink wraps an arbitrary function as an EventSink.
type funcSink struct {
	fn func(ctx context.Context, ev monitor.Event) error
}

func (s *funcSink) HandleEvent(ctx context.Context, ev monitor.Event) error {
	return s.fn(ctx, ev)
}

// errSink always returns the configured error.
type errSink struct {
	err error
}

func (s *errSink) HandleEvent(_ context.Context, _ monitor.Event) error {
	return s.err
}

func testClock() *clock.Mock {
	return clock.NewMock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
}

// --- New tests ---

func TestNew_RequiresSources(t *testing.T) {
	t.Parallel()
	_, err := monitor.New()
	if err == nil {
		t.Fatal("expected error when no sources provided, got nil")
	}
}

func TestNew_SucceedsWithSource(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(testClock()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil monitor")
	}
}

// --- Snapshot tests ---

func TestSnapshot_EmptyMonitor(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(testClock()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d sessions", len(snap))
	}
}

func TestSnapshot_CloneIndependence(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:     "s1",
		Activity:      session.ActivityWorking,
		Model:         "opus",
		ContextTokens: 500,
	}, "c1")

	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap1 := m.Snapshot()
	if len(snap1) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap1))
	}

	// Mutate the snapshot.
	snap1[0].ID = "mutated"
	snap1[0].Activity = session.ActivityIdle

	// Take another snapshot — must not reflect mutations.
	snap2 := m.Snapshot()
	if len(snap2) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap2))
	}
	if snap2[0].ID != "s1" {
		t.Errorf("snapshot ID = %q, want %q", snap2[0].ID, "s1")
	}
	if snap2[0].Activity != session.ActivityWorking {
		t.Errorf("snapshot Activity = %q, want %q", snap2[0].Activity, session.ActivityWorking)
	}
}

// --- PollOnce: single source, single session ---

func TestPollOnce_SingleSourceSingleSession(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("claude"))
	started := clk.Now().Add(-time.Minute)
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude", WorkingDir: "/project", StartedAt: started},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:     "s1",
		Activity:      session.ActivityWorking,
		Model:         "opus",
		ContextTokens: 1000,
		MaxContextTokens: 200000,
		WorkingDir:    "/project",
		Branch:        "main",
		LastActivityAt: clk.Now(),
		MessageCountDelta:  1,
		ToolCallCountDelta: 2,
		CurrentTool:   "bash",
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}

	s := snap[0]
	if s.ID != "s1" {
		t.Errorf("ID = %q, want %q", s.ID, "s1")
	}
	if s.Source != "claude" {
		t.Errorf("Source = %q, want %q", s.Source, "claude")
	}
	if s.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want %q", s.Activity, session.ActivityWorking)
	}
	if s.Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q", s.Lifecycle, session.LifecycleActive)
	}
	if s.Model != "opus" {
		t.Errorf("Model = %q, want %q", s.Model, "opus")
	}
	if s.ContextTokens != 1000 {
		t.Errorf("ContextTokens = %d, want 1000", s.ContextTokens)
	}
	if s.MaxContextTokens != 200000 {
		t.Errorf("MaxContextTokens = %d, want 200000", s.MaxContextTokens)
	}
	if s.WorkingDir != "/project" {
		t.Errorf("WorkingDir = %q, want %q", s.WorkingDir, "/project")
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want %q", s.Branch, "main")
	}
	if s.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", s.MessageCount)
	}
	if s.ToolCallCount != 2 {
		t.Errorf("ToolCallCount = %d, want 2", s.ToolCallCount)
	}
	if s.CurrentTool != "bash" {
		t.Errorf("CurrentTool = %q, want %q", s.CurrentTool, "bash")
	}
	if !s.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", s.StartedAt, started)
	}
	if !s.LastDataReceivedAt.Equal(clk.Now()) {
		t.Errorf("LastDataReceivedAt = %v, want %v", s.LastDataReceivedAt, clk.Now())
	}
}

// --- PollOnce: multi-source ---

func TestPollOnce_MultiSource(t *testing.T) {
	t.Parallel()
	clk := testClock()

	src1 := mock.New(mock.WithName("claude"))
	src1.SetHandles([]source.SessionHandle{
		{ID: "claude-1", Source: "claude", StartedAt: clk.Now()},
	})
	src1.AddUpdate("claude-1", source.SourceUpdate{
		SessionID: "claude-1",
		Activity:  session.ActivityWorking,
		Model:     "opus",
	}, "c1")

	src2 := mock.New(mock.WithName("codex"))
	src2.SetHandles([]source.SessionHandle{
		{ID: "codex-1", Source: "codex", StartedAt: clk.Now()},
	})
	src2.AddUpdate("codex-1", source.SourceUpdate{
		SessionID: "codex-1",
		Activity:  session.ActivityIdle,
		Model:     "o3",
	}, "x1")

	m, err := monitor.New(
		monitor.WithSources(src1, src2),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(snap))
	}

	byID := make(map[string]session.SessionState)
	for i := 0; i < len(snap); i++ {
		byID[snap[i].ID] = snap[i]
	}

	if s, ok := byID["claude-1"]; !ok {
		t.Error("missing session claude-1")
	} else if s.Model != "opus" {
		t.Errorf("claude-1 Model = %q, want %q", s.Model, "opus")
	}

	if s, ok := byID["codex-1"]; !ok {
		t.Error("missing session codex-1")
	} else if s.Model != "o3" {
		t.Errorf("codex-1 Model = %q, want %q", s.Model, "o3")
	}
}

// --- PollOnce: update propagation with delta accumulation ---

func TestPollOnce_UpdatePropagation(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First poll: initial data.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:          "s1",
		Activity:           session.ActivityWorking,
		Model:              "opus",
		ContextTokens:      1000,
		MaxContextTokens:   200000,
		MessageCountDelta:  3,
		ToolCallCountDelta: 5,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Second poll: updated data with deltas.
	clk.Advance(5 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:          "s1",
		Activity:           session.ActivityIdle,
		ContextTokens:      2000,
		MessageCountDelta:  2,
		ToolCallCountDelta: 1,
		CurrentTool:        "read",
	}, "c2")

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}

	s := snap[0]
	if s.Activity != session.ActivityIdle {
		t.Errorf("Activity = %q, want %q", s.Activity, session.ActivityIdle)
	}
	if s.ContextTokens != 2000 {
		t.Errorf("ContextTokens = %d, want 2000", s.ContextTokens)
	}
	// Deltas accumulate: 3+2=5 messages, 5+1=6 tool calls.
	if s.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", s.MessageCount)
	}
	if s.ToolCallCount != 6 {
		t.Errorf("ToolCallCount = %d, want 6", s.ToolCallCount)
	}
	if s.CurrentTool != "read" {
		t.Errorf("CurrentTool = %q, want %q", s.CurrentTool, "read")
	}
	// Model persists from first update.
	if s.Model != "opus" {
		t.Errorf("Model = %q, want %q (should persist)", s.Model, "opus")
	}
}

// --- PollOnce: context utilization ---

func TestPollOnce_ContextUtilization(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:        "s1",
		Activity:         session.ActivityWorking,
		ContextTokens:    50000,
		MaxContextTokens: 200000,
	}, "c1")

	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}

	want := 0.25 // 50000 / 200000
	if snap[0].ContextUtilization != want {
		t.Errorf("ContextUtilization = %v, want %v", snap[0].ContextUtilization, want)
	}
}

// --- PollOnce: terminal transition ---

func TestPollOnce_TerminalTransition(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First poll: active.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Second poll: terminal.
	clk.Advance(10 * time.Second)
	endedAt := clk.Now()
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "completed",
		EndedAt:   endedAt,
	}, "c2")

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}

	s := snap[0]
	if s.Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want %q", s.Lifecycle, session.LifecycleTerminal)
	}
	if s.CompletedAt == nil {
		t.Fatal("CompletedAt should not be nil")
	}
	if !s.CompletedAt.Equal(endedAt) {
		t.Errorf("CompletedAt = %v, want %v", *s.CompletedAt, endedAt)
	}

	// Check lifecycle event was delivered.
	var found bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventTerminal {
			found = true
			if ev.Lifecycle.Reason != "completed" {
				t.Errorf("Lifecycle.Reason = %q, want %q", ev.Lifecycle.Reason, "completed")
			}
		}
	}
	if !found {
		t.Error("no terminal lifecycle event delivered")
	}
}

// --- PollOnce: resumed from terminal ---

func TestPollOnce_ResumedFromTerminal(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Poll 1: active.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Poll 2: terminal.
	clk.Advance(5 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if snap[0].Lifecycle != session.LifecycleTerminal {
		t.Fatalf("expected terminal, got %q", snap[0].Lifecycle)
	}

	// Poll 3: resumed with new data.
	clk.Advance(5 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c3")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleActive)
	}
	if snap[0].CompletedAt != nil {
		t.Error("CompletedAt should be nil after resume")
	}

	// Verify resumed lifecycle event.
	var resumeFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventResumed {
			resumeFound = true
			if ev.Lifecycle.From != session.LifecycleTerminal {
				t.Errorf("resume From = %q, want %q", ev.Lifecycle.From, session.LifecycleTerminal)
			}
			if ev.Lifecycle.To != session.LifecycleActive {
				t.Errorf("resume To = %q, want %q", ev.Lifecycle.To, session.LifecycleActive)
			}
		}
	}
	if !resumeFound {
		t.Error("no resumed lifecycle event delivered")
	}
}

// --- PollOnce: no new data is a no-op ---

func TestPollOnce_NoNewData(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	// No updates queued — Parse will return zero SourceUpdate.

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(snap))
	}
	if len(sink.events) != 0 {
		t.Errorf("expected 0 events, got %d", len(sink.events))
	}
}

// --- PollOnce: lifecycle event delivery ---

func TestPollOnce_LifecycleEventDelivery(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	// Expect one lifecycle event (discovered) and one delta event.
	if len(sink.events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(sink.events))
	}

	lcEv := sink.events[0]
	if lcEv.Type != monitor.EventLifecycle {
		t.Errorf("first event type = %q, want %q", lcEv.Type, monitor.EventLifecycle)
	}
	if lcEv.Lifecycle == nil {
		t.Fatal("lifecycle event has nil Lifecycle")
	}
	if lcEv.Lifecycle.Type != session.EventDiscovered {
		t.Errorf("lifecycle type = %q, want %q", lcEv.Lifecycle.Type, session.EventDiscovered)
	}
	if lcEv.Lifecycle.SessionID != "s1" {
		t.Errorf("lifecycle SessionID = %q, want %q", lcEv.Lifecycle.SessionID, "s1")
	}

	deltaEv := sink.events[1]
	if deltaEv.Type != monitor.EventDelta {
		t.Errorf("second event type = %q, want %q", deltaEv.Type, monitor.EventDelta)
	}
	if len(deltaEv.Updates) != 1 {
		t.Fatalf("delta Updates length = %d, want 1", len(deltaEv.Updates))
	}
	if deltaEv.Updates[0].ID != "s1" {
		t.Errorf("delta Update ID = %q, want %q", deltaEv.Updates[0].ID, "s1")
	}
}

// --- PollOnce: event sequence numbers ---

func TestPollOnce_EventSequencing(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(sink.events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(sink.events))
	}

	// Sequence numbers must be monotonically increasing.
	var prevSeq uint64
	for i := 0; i < len(sink.events); i++ {
		if sink.events[i].Seq <= prevSeq {
			t.Errorf("event[%d].Seq = %d, not greater than previous %d",
				i, sink.events[i].Seq, prevSeq)
		}
		prevSeq = sink.events[i].Seq
	}
}

// --- PollOnce: state committed before sink call (§12.8 invariant) ---

func TestPollOnce_StateCommittedBeforeSinkCall(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	var mon *monitor.Monitor
	var snapshotDuringSink []session.SessionState

	sink := &funcSink{fn: func(_ context.Context, ev monitor.Event) error {
		// During the first lifecycle event, take a snapshot.
		// If the lock were still held, this would deadlock.
		// If state weren't committed, the snapshot would be empty.
		if ev.Type == monitor.EventLifecycle {
			snapshotDuringSink = mon.Snapshot()
		}
		return nil
	}}

	var err error
	mon, err = monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(snapshotDuringSink) != 1 {
		t.Fatalf("snapshot during sink had %d sessions, want 1 (state must be committed before sink call)",
			len(snapshotDuringSink))
	}
	if snapshotDuringSink[0].ID != "s1" {
		t.Errorf("snapshot session ID = %q, want %q", snapshotDuringSink[0].ID, "s1")
	}
}

// --- MultiSink tests ---

func TestMultiSink_DeliversToAll(t *testing.T) {
	t.Parallel()
	s1 := &recordingSink{}
	s2 := &recordingSink{}
	multi := monitor.NewMultiSink(s1, s2)

	ev := monitor.Event{Seq: 1, Type: monitor.EventDelta}
	err := multi.HandleEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(s1.events) != 1 {
		t.Errorf("sink1 got %d events, want 1", len(s1.events))
	}
	if len(s2.events) != 1 {
		t.Errorf("sink2 got %d events, want 1", len(s2.events))
	}
}

func TestMultiSink_JoinsErrors(t *testing.T) {
	t.Parallel()
	err1 := errors.New("sink1 failed")
	err2 := errors.New("sink2 failed")
	multi := monitor.NewMultiSink(
		&errSink{err: err1},
		&recordingSink{},
		&errSink{err: err2},
	)

	err := multi.HandleEvent(context.Background(), monitor.Event{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, err1) {
		t.Errorf("error should wrap err1: %v", err)
	}
	if !errors.Is(err, err2) {
		t.Errorf("error should wrap err2: %v", err)
	}
}

func TestMultiSink_ContinuesAfterError(t *testing.T) {
	t.Parallel()
	recorder := &recordingSink{}
	multi := monitor.NewMultiSink(
		&errSink{err: errors.New("fail")},
		recorder,
	)

	_ = multi.HandleEvent(context.Background(), monitor.Event{Seq: 1})
	if len(recorder.events) != 1 {
		t.Errorf("second sink should still receive event; got %d events", len(recorder.events))
	}
}

// --- PollOnce: subagents propagated ---

func TestPollOnce_SubagentsPropagated(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "sub-1", Activity: session.ActivityWorking, StartedAt: clk.Now()},
			{ID: "sub-2", Activity: session.ActivityIdle, StartedAt: clk.Now()},
		},
	}, "c1")

	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if len(snap[0].Subagents) != 2 {
		t.Fatalf("expected 2 subagents, got %d", len(snap[0].Subagents))
	}
	if snap[0].Subagents[0].ID != "sub-1" {
		t.Errorf("subagent[0].ID = %q, want %q", snap[0].Subagents[0].ID, "sub-1")
	}
	if snap[0].Subagents[1].ID != "sub-2" {
		t.Errorf("subagent[1].ID = %q, want %q", snap[0].Subagents[1].ID, "sub-2")
	}
}

// --- PollOnce: terminal with default EndedAt ---

func TestPollOnce_TerminalDefaultEndedAt(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Active first.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Terminal with zero EndedAt — should default to clock.Now().
	clk.Advance(10 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "timeout",
		// EndedAt deliberately zero.
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if snap[0].CompletedAt == nil {
		t.Fatal("CompletedAt should not be nil")
	}
	if !snap[0].CompletedAt.Equal(clk.Now()) {
		t.Errorf("CompletedAt = %v, want %v (clock.Now fallback)", *snap[0].CompletedAt, clk.Now())
	}
}

// --- Lifecycle transition table tests (§4.5) ---

// TestTransition_ActiveToTerminal_Stale verifies that an active session with no
// data past the stale threshold transitions to terminal with a "stale" event.
func TestTransition_ActiveToTerminal_Stale(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First poll: discover the session.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	staleThreshold := 30 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(staleThreshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Lifecycle != session.LifecycleActive {
		t.Fatalf("expected 1 active session, got %d with lifecycle %q", len(snap), snap[0].Lifecycle)
	}

	// Advance past stale threshold with no new data.
	clk.Advance(staleThreshold + time.Second)
	// No new update queued — Parse returns zero SourceUpdate.

	sink.events = nil // reset events
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleTerminal)
	}
	if snap[0].CompletedAt == nil {
		t.Fatal("CompletedAt should not be nil after stale transition")
	}

	// Verify stale lifecycle event was delivered.
	var staleFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventStale {
			staleFound = true
			if ev.Lifecycle.From != session.LifecycleActive {
				t.Errorf("stale From = %q, want %q", ev.Lifecycle.From, session.LifecycleActive)
			}
			if ev.Lifecycle.To != session.LifecycleTerminal {
				t.Errorf("stale To = %q, want %q", ev.Lifecycle.To, session.LifecycleTerminal)
			}
			if ev.Lifecycle.SessionID != "s1" {
				t.Errorf("stale SessionID = %q, want %q", ev.Lifecycle.SessionID, "s1")
			}
		}
	}
	if !staleFound {
		t.Error("no stale lifecycle event delivered")
	}
}

// TestTransition_ActiveNotStale_WithinThreshold verifies that an active session
// within the stale threshold is not marked stale.
func TestTransition_ActiveNotStale_WithinThreshold(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	staleThreshold := 30 * time.Second
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(staleThreshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Advance but stay within threshold.
	clk.Advance(staleThreshold - time.Second)

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q (should still be active)", snap[0].Lifecycle, session.LifecycleActive)
	}
}

// TestTransition_StaleDisabledWhenZero verifies that a zero stale threshold
// disables stale detection.
func TestTransition_StaleDisabledWhenZero(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(0), // disabled
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Advance way past any reasonable threshold.
	clk.Advance(24 * time.Hour)

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q (stale disabled)", snap[0].Lifecycle, session.LifecycleActive)
	}
}

// TestTransition_TerminalToRemoved verifies that a terminal session is removed
// from the store after the retention window expires.
func TestTransition_TerminalToRemoved(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Poll 1: discover.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	retention := 10 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(retention),
		monitor.WithStaleThreshold(0), // disable stale so it doesn't interfere
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Poll 2: terminal.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Lifecycle != session.LifecycleTerminal {
		t.Fatalf("expected 1 terminal session, got %d", len(snap))
	}

	// Advance past retention window.
	clk.Advance(retention + time.Second)
	sink.events = nil

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	// Session should be removed from store.
	snap = m.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions after retention expires, got %d", len(snap))
	}

	// Verify removed lifecycle event was delivered.
	var removedFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventRemoved {
			removedFound = true
			if ev.Lifecycle.From != session.LifecycleTerminal {
				t.Errorf("removed From = %q, want %q", ev.Lifecycle.From, session.LifecycleTerminal)
			}
			if ev.Lifecycle.SessionID != "s1" {
				t.Errorf("removed SessionID = %q, want %q", ev.Lifecycle.SessionID, "s1")
			}
		}
	}
	if !removedFound {
		t.Error("no removed lifecycle event delivered")
	}

	// Verify delta event includes removed ID.
	var deltaFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventDelta {
			deltaFound = true
			if len(ev.Removed) != 1 || ev.Removed[0] != "s1" {
				t.Errorf("delta Removed = %v, want [s1]", ev.Removed)
			}
		}
	}
	if !deltaFound {
		t.Error("no delta event with removed IDs delivered")
	}
}

// TestTransition_TerminalNotRemovedBeforeRetention verifies that a terminal
// session is retained within the retention window.
func TestTransition_TerminalNotRemovedBeforeRetention(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	retention := 30 * time.Second
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(retention),
		monitor.WithStaleThreshold(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Terminal transition.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Advance but stay within retention.
	clk.Advance(retention - 2*time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session (still retained), got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleTerminal)
	}
}

// TestTransition_RemovedToActive_Resumed verifies that a removed session
// returning new data transitions to active with a "resumed" event.
func TestTransition_RemovedToActive_Resumed(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Poll 1: discover.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	retention := 5 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(retention),
		monitor.WithStaleThreshold(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Poll 2: terminal.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Poll 3: advance past retention → removed.
	clk.Advance(retention + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected 0 sessions after removal, got %d", len(snap))
	}

	// Poll 4: new data for the same session ID → resumed from removed.
	clk.Advance(time.Second)
	sink.events = nil
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Model:     "opus-2",
	}, "c3")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 4: %v", err)
	}

	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session after resume from removed, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleActive)
	}
	if snap[0].Model != "opus-2" {
		t.Errorf("Model = %q, want %q", snap[0].Model, "opus-2")
	}
	if snap[0].CompletedAt != nil {
		t.Error("CompletedAt should be nil after resume")
	}

	// Verify resumed lifecycle event.
	var resumeFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventResumed {
			resumeFound = true
			if ev.Lifecycle.From != session.LifecycleTerminal {
				t.Errorf("resume From = %q, want %q", ev.Lifecycle.From, session.LifecycleTerminal)
			}
			if ev.Lifecycle.To != session.LifecycleActive {
				t.Errorf("resume To = %q, want %q", ev.Lifecycle.To, session.LifecycleActive)
			}
		}
	}
	if !resumeFound {
		t.Error("no resumed lifecycle event delivered for removed → active")
	}
}

// TestTransition_RemovedNoNewData verifies that a removed session with no
// new data remains absent from the store and emits no events.
func TestTransition_RemovedNoNewData(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Discover → terminal → removed.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	retention := 5 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(retention),
		monitor.WithStaleThreshold(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	clk.Advance(retention + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	// Now removed. Poll again with no new data.
	sink.events = nil
	clk.Advance(time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 4: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(snap))
	}
	if len(sink.events) != 0 {
		t.Errorf("expected 0 events for removed+no-data, got %d", len(sink.events))
	}
}

// TestTransition_StaleOnlyAffectsActiveSessions verifies that the stale sweep
// does not affect terminal sessions.
func TestTransition_StaleOnlyAffectsActiveSessions(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Discover and then immediately terminal.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	staleThreshold := 10 * time.Second
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(staleThreshold),
		monitor.WithCompletionRetention(time.Hour), // long retention so it doesn't get removed
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Terminal.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Advance past stale threshold — should NOT trigger another stale event.
	clk.Advance(staleThreshold + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	// Should still be terminal, not double-terminalled.
	if snap[0].Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleTerminal)
	}
}

// TestTransition_FullLifecycle exercises the complete path:
// untracked → active → active → terminal → (retained) → removed → resumed.
func TestTransition_FullLifecycle(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	retention := 10 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(retention),
		monitor.WithStaleThreshold(0), // use explicit terminal, not stale
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Step 1: discovered.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Model:     "opus",
	}, "c1")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce discovered: %v", err)
	}

	// Step 2: updated.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:     "s1",
		Activity:      session.ActivityIdle,
		ContextTokens: 500,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce updated: %v", err)
	}

	// Step 3: terminal.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "user exit",
	}, "c3")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce terminal: %v", err)
	}

	// Step 4: still retained.
	clk.Advance(retention / 2)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce retained: %v", err)
	}
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 retained session, got %d", len(snap))
	}

	// Step 5: removed.
	clk.Advance(retention)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce removed: %v", err)
	}
	snap = m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected 0 sessions after removal, got %d", len(snap))
	}

	// Step 6: resumed from removed.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Model:     "opus-2",
	}, "c4")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce resumed: %v", err)
	}
	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session after resume, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("final Lifecycle = %q, want %q", snap[0].Lifecycle, session.LifecycleActive)
	}

	// Collect all lifecycle event types in order.
	var lcTypes []session.LifecycleEventType
	for i := 0; i < len(sink.events); i++ {
		if sink.events[i].Type == monitor.EventLifecycle && sink.events[i].Lifecycle != nil {
			lcTypes = append(lcTypes, sink.events[i].Lifecycle.Type)
		}
	}

	expected := []session.LifecycleEventType{
		session.EventDiscovered,
		session.EventUpdated,
		session.EventTerminal,
		session.EventRemoved,
		session.EventResumed,
	}

	if len(lcTypes) != len(expected) {
		t.Fatalf("lifecycle events = %v, want %v", lcTypes, expected)
	}
	for i := 0; i < len(expected); i++ {
		if lcTypes[i] != expected[i] {
			t.Errorf("lifecycle[%d] = %q, want %q", i, lcTypes[i], expected[i])
		}
	}
}

// TestTransition_StaleResetsOnNewData verifies that receiving data resets the
// stale clock — a session that almost went stale but gets an update stays active.
func TestTransition_StaleResetsOnNewData(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	staleThreshold := 30 * time.Second
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(staleThreshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Almost stale.
	clk.Advance(staleThreshold - time.Second)

	// New data arrives — resets LastDataReceivedAt.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityIdle,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Advance again — now from the reset point, not the original.
	clk.Advance(staleThreshold - time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q (data reset stale clock)", snap[0].Lifecycle, session.LifecycleActive)
	}
}

// TestTransition_MultipleSessionsIndependentLifecycles verifies that lifecycle
// transitions are tracked independently per session.
func TestTransition_MultipleSessionsIndependentLifecycles(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
		{ID: "s2", Source: "test", StartedAt: clk.Now()},
	})

	// Both start active.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")
	src.AddUpdate("s2", source.SourceUpdate{
		SessionID: "s2",
		Activity:  session.ActivityIdle,
	}, "c1")

	staleThreshold := 20 * time.Second
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(staleThreshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// s1 gets new data; s2 does not.
	clk.Advance(10 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityIdle,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Advance past stale threshold for s2 but not s1.
	clk.Advance(11 * time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	byID := make(map[string]session.SessionState)
	for i := 0; i < len(snap); i++ {
		byID[snap[i].ID] = snap[i]
	}

	if s1, ok := byID["s1"]; !ok {
		t.Error("s1 missing from snapshot")
	} else if s1.Lifecycle != session.LifecycleActive {
		t.Errorf("s1 Lifecycle = %q, want %q", s1.Lifecycle, session.LifecycleActive)
	}

	if s2, ok := byID["s2"]; !ok {
		t.Error("s2 missing from snapshot")
	} else if s2.Lifecycle != session.LifecycleTerminal {
		t.Errorf("s2 Lifecycle = %q, want %q (should be stale)", s2.Lifecycle, session.LifecycleTerminal)
	}
}
