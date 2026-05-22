package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
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

// findLifecycleEvent searches events for a lifecycle event of the given type.
// Returns the event and true if found, or zero Event and false otherwise.
func findLifecycleEvent(events []monitor.Event, lcType session.LifecycleEventType) (monitor.Event, bool) {
	for i := 0; i < len(events); i++ {
		ev := events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == lcType {
			return ev, true
		}
	}
	return monitor.Event{}, false
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

// --- PollOnce: compaction accumulation ---

func TestPollOnce_CompactionAccumulation(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First poll: one compaction event.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:            "s1",
		Activity:             session.ActivityWorking,
		CompactionCountDelta: 1,
	}, "c1")

	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].CompactionCount != 1 {
		t.Errorf("CompactionCount after first poll = %d, want 1", snap[0].CompactionCount)
	}

	// Second poll: two more compaction events.
	clk.Advance(5 * time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID:            "s1",
		Activity:             session.ActivityWorking,
		CompactionCountDelta: 2,
	}, "c2")

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	// Accumulated: 1 + 2 = 3.
	if snap[0].CompactionCount != 3 {
		t.Errorf("CompactionCount after second poll = %d, want 3", snap[0].CompactionCount)
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
	ev, found := findLifecycleEvent(sink.events, session.EventTerminal)
	if !found {
		t.Fatal("no terminal lifecycle event delivered")
	}
	if ev.Lifecycle.Reason != "completed" {
		t.Errorf("Lifecycle.Reason = %q, want %q", ev.Lifecycle.Reason, "completed")
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
	resumedEv, found := findLifecycleEvent(sink.events, session.EventResumed)
	if !found {
		t.Fatal("no resumed lifecycle event delivered")
	}
	if resumedEv.Lifecycle.From != session.LifecycleTerminal {
		t.Errorf("resume From = %q, want %q", resumedEv.Lifecycle.From, session.LifecycleTerminal)
	}
	if resumedEv.Lifecycle.To != session.LifecycleActive {
		t.Errorf("resume To = %q, want %q", resumedEv.Lifecycle.To, session.LifecycleActive)
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

	// Poll 3: advance past retention -> removed.
	clk.Advance(retention + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected 0 sessions after removal, got %d", len(snap))
	}

	// Poll 4: new data for the same session ID -> resumed from removed.
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
		t.Error("no resumed lifecycle event delivered for removed -> active")
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

	// Discover -> terminal -> removed.
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
// untracked -> active -> active -> terminal -> (retained) -> removed -> resumed.
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

// --- Run integration tests ---

// yieldToRun gives the Run goroutine time to start and create its ticker.
// Without this, clk.Advance may fire before the ticker exists.
func yieldToRun() {
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
}

// waitForEvent reads from eventCh with a real-time deadline as a safety net.
func waitForEvent(t *testing.T, eventCh <-chan monitor.Event) monitor.Event {
	t.Helper()
	select {
	case ev := <-eventCh:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return monitor.Event{}
	}
}

// waitForDone waits for the Run goroutine to finish.
func waitForDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run to return")
		return nil
	}
}

func TestRun_TickDrivesPollOnce(t *testing.T) {
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

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Trigger first tick.
	clk.Advance(time.Second)

	// Expect lifecycle(discovered) + delta from first poll.
	ev1 := waitForEvent(t, eventCh)
	if ev1.Type != monitor.EventLifecycle {
		t.Errorf("first event type = %q, want %q", ev1.Type, monitor.EventLifecycle)
	}
	ev2 := waitForEvent(t, eventCh)
	if ev2.Type != monitor.EventDelta {
		t.Errorf("second event type = %q, want %q", ev2.Type, monitor.EventDelta)
	}

	// Snapshot should show the session.
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].ID != "s1" {
		t.Errorf("session ID = %q, want %q", snap[0].ID, "s1")
	}

	cancel()
	runErr := waitForDone(t, done)
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", runErr)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Cancel immediately — Run should exit without needing a tick.
	cancel()

	runErr := waitForDone(t, done)
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", runErr)
	}
}

func TestRun_MultipleTicks(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First tick: discover.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1: discover s1.
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle
	_ = waitForEvent(t, eventCh) // delta

	// Queue second update and tick.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityIdle,
	}, "c2")
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(updated)
	_ = waitForEvent(t, eventCh) // delta

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].Activity != session.ActivityIdle {
		t.Errorf("Activity = %q, want %q", snap[0].Activity, session.ActivityIdle)
	}

	cancel()
	_ = waitForDone(t, done)
}

func TestRun_CompletionRetention_RemovesTerminalSession(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Tick 1: active.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
		monitor.WithCompletionRetention(10*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1: discover.
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(discovered)
	_ = waitForEvent(t, eventCh) // delta

	// Tick 2: terminal.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "completed",
	}, "c2")
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(terminal)
	_ = waitForEvent(t, eventCh) // delta

	// Session is terminal but within retention.
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session still in store, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want terminal", snap[0].Lifecycle)
	}

	// Advance past retention (10s) and trigger tick.
	clk.Advance(11 * time.Second)

	// reapTerminal emits EventRemoved.
	ev := waitForEvent(t, eventCh)
	if ev.Type != monitor.EventLifecycle {
		t.Fatalf("expected lifecycle event, got %q", ev.Type)
	}
	if ev.Lifecycle == nil {
		t.Fatal("lifecycle event has nil Lifecycle")
	}
	if ev.Lifecycle.Type != session.EventRemoved {
		t.Errorf("lifecycle type = %q, want %q", ev.Lifecycle.Type, session.EventRemoved)
	}
	if ev.Lifecycle.SessionID != "s1" {
		t.Errorf("lifecycle SessionID = %q, want %q", ev.Lifecycle.SessionID, "s1")
	}
	if ev.Lifecycle.From != session.LifecycleTerminal {
		t.Errorf("lifecycle From = %q, want %q", ev.Lifecycle.From, session.LifecycleTerminal)
	}

	// Session should be gone from snapshot.
	snap = m.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions after retention, got %d", len(snap))
	}

	cancel()
	_ = waitForDone(t, done)
}

func TestRun_RetentionNotExpired_SessionRetained(t *testing.T) {
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

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
		monitor.WithCompletionRetention(30*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1: discover.
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle
	_ = waitForEvent(t, eventCh) // delta

	// Tick 2: terminal.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(terminal)
	_ = waitForEvent(t, eventCh) // delta

	// Advance within retention (only 5s, retention is 30s).
	clk.Advance(5 * time.Second)

	// Give Run a moment to process the tick (no events expected for no-data poll).
	time.Sleep(50 * time.Millisecond)

	// Session should still be in store.
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Errorf("expected 1 session (within retention), got %d", len(snap))
	}

	cancel()
	_ = waitForDone(t, done)
}

func TestRun_RemovedSessionResumes(t *testing.T) {
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

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
		monitor.WithCompletionRetention(5*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1: discover s1 active.
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(discovered)
	_ = waitForEvent(t, eventCh) // delta

	// Tick 2: terminal.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh) // lifecycle(terminal)
	_ = waitForEvent(t, eventCh) // delta

	// Tick 3: advance past retention -> reap.
	clk.Advance(6 * time.Second)
	removedEv := waitForEvent(t, eventCh) // lifecycle(removed)
	if removedEv.Lifecycle.Type != session.EventRemoved {
		t.Fatalf("expected removed event, got %q", removedEv.Lifecycle.Type)
	}
	_ = waitForEvent(t, eventCh) // delta(Removed)

	// Snapshot should be empty.
	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("expected 0 sessions after removal, got %d", len(snap))
	}

	// Queue new data for the removed session.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Model:     "sonnet",
	}, "c3")

	// Tick 4: session reappears -> should be "resumed", not "discovered".
	clk.Advance(time.Second)
	resumedEv := waitForEvent(t, eventCh) // lifecycle(resumed)
	if resumedEv.Type != monitor.EventLifecycle {
		t.Fatalf("expected lifecycle event, got %q", resumedEv.Type)
	}
	if resumedEv.Lifecycle.Type != session.EventResumed {
		t.Errorf("lifecycle type = %q, want %q", resumedEv.Lifecycle.Type, session.EventResumed)
	}
	if resumedEv.Lifecycle.From != session.LifecycleTerminal {
		t.Errorf("lifecycle From = %q, want %q", resumedEv.Lifecycle.From, session.LifecycleTerminal)
	}
	if resumedEv.Lifecycle.To != session.LifecycleActive {
		t.Errorf("lifecycle To = %q, want %q", resumedEv.Lifecycle.To, session.LifecycleActive)
	}

	_ = waitForEvent(t, eventCh) // delta

	// Session should be back in snapshot.
	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session after resume, got %d", len(snap))
	}
	if snap[0].Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want active", snap[0].Lifecycle)
	}
	if snap[0].Model != "sonnet" {
		t.Errorf("Model = %q, want %q", snap[0].Model, "sonnet")
	}

	cancel()
	_ = waitForDone(t, done)
}

func TestRun_EventSeqMonotonicAcrossTicks(t *testing.T) {
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

	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1.
	clk.Advance(time.Second)
	ev1 := waitForEvent(t, eventCh) // lifecycle
	ev2 := waitForEvent(t, eventCh) // delta

	// Tick 2 with update.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityIdle,
	}, "c2")
	clk.Advance(time.Second)
	ev3 := waitForEvent(t, eventCh) // lifecycle(updated)
	ev4 := waitForEvent(t, eventCh) // delta

	// All seq numbers must be strictly increasing.
	seqs := []uint64{ev1.Seq, ev2.Seq, ev3.Seq, ev4.Seq}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq[%d]=%d not greater than seq[%d]=%d",
				i, seqs[i], i-1, seqs[i-1])
		}
	}

	cancel()
	_ = waitForDone(t, done)
}

func TestRun_CompletionRetention_WithRemovalEvent(t *testing.T) {
	// Verify that the EventRemoved seq is consistent with other events.
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

	var allSeqs []uint64
	var mu sync.Mutex
	eventCh := make(chan monitor.Event, 100)
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		mu.Lock()
		allSeqs = append(allSeqs, ev.Seq)
		mu.Unlock()
		eventCh <- ev
		return nil
	})

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
		monitor.WithCompletionRetention(5*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Tick 1: discover.
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh)
	_ = waitForEvent(t, eventCh)

	// Tick 2: terminal.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Terminal:  true,
		EndReason: "done",
	}, "c2")
	clk.Advance(time.Second)
	_ = waitForEvent(t, eventCh)
	_ = waitForEvent(t, eventCh)

	// Tick 3: reap.
	clk.Advance(6 * time.Second)
	_ = waitForEvent(t, eventCh) // removed

	cancel()
	_ = waitForDone(t, done)

	// All seq numbers must be strictly increasing.
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(allSeqs); i++ {
		if allSeqs[i] <= allSeqs[i-1] {
			t.Errorf("seq[%d]=%d not greater than seq[%d]=%d",
				i, allSeqs[i], i-1, allSeqs[i-1])
		}
	}
}

// --- Subagent cross-session completion tests ---

// TestPollOnce_SubagentCompletionMarksMatchingSession verifies that when a
// parent session reports a completed subagent (via SubagentState with
// ActivityTerminal and a slug), a separate active session with that same slug
// is marked terminal immediately — not after the stale threshold.
func TestPollOnce_SubagentCompletionMarksMatchingSession(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("claude"))

	// Two sessions: a parent and a subagent (with its own independent session).
	src.SetHandles([]source.SessionHandle{
		{ID: "parent-1", Source: "claude", StartedAt: clk.Now()},
		{ID: "subagent-1", Source: "claude", StartedAt: clk.Now()},
	})

	// Poll 1: both sessions discovered and active.
	src.AddUpdate("parent-1", source.SourceUpdate{
		SessionID: "parent-1",
		Slug:      "orchestrator",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "explore-task", Activity: session.ActivityWorking},
		},
	}, "c1")
	src.AddUpdate("subagent-1", source.SourceUpdate{
		SessionID: "subagent-1",
		Slug:      "explore-task",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(5*time.Minute), // long stale — should NOT be needed
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(snap))
	}

	// Poll 2: parent reports the subagent as completed (tool_result received).
	// The subagent's own session has no new data — Parse returns zero update.
	clk.Advance(5 * time.Second)
	sink.events = nil

	src.AddUpdate("parent-1", source.SourceUpdate{
		SessionID: "parent-1",
		Slug:      "orchestrator",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "explore-task", Activity: session.ActivityTerminal},
		},
	}, "c2")
	// No update for subagent-1 — it's gone quiet.

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// The subagent session should now be terminal — NOT waiting 5 minutes.
	snap = m.Snapshot()
	byID := make(map[string]session.SessionState)
	for i := 0; i < len(snap); i++ {
		byID[snap[i].ID] = snap[i]
	}

	parent := byID["parent-1"]
	if parent.Lifecycle != session.LifecycleActive {
		t.Errorf("parent Lifecycle = %q, want active", parent.Lifecycle)
	}

	sub := byID["subagent-1"]
	if sub.Lifecycle != session.LifecycleTerminal {
		t.Errorf("subagent Lifecycle = %q, want terminal (should be marked by completion sweep)", sub.Lifecycle)
	}
	if sub.CompletedAt == nil {
		t.Error("subagent CompletedAt should not be nil")
	}

	// Verify terminal lifecycle event was delivered for the subagent.
	var terminalFound bool
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil &&
			ev.Lifecycle.Type == session.EventTerminal &&
			ev.Lifecycle.SessionID == "subagent-1" {
			terminalFound = true
			if ev.Lifecycle.Reason != "parent reported subagent completion" {
				t.Errorf("terminal reason = %q", ev.Lifecycle.Reason)
			}
		}
	}
	if !terminalFound {
		t.Error("no terminal lifecycle event delivered for completed subagent")
	}
}

// TestPollOnce_SubagentCompletion_NoFalsePositives verifies that the
// subagent completion sweep does NOT mark a session terminal just because
// it shares a slug with a working (not completed) subagent.
func TestPollOnce_SubagentCompletion_NoFalsePositives(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))

	src.SetHandles([]source.SessionHandle{
		{ID: "parent-1", Source: "test", StartedAt: clk.Now()},
		{ID: "sub-1", Source: "test", StartedAt: clk.Now()},
	})

	// Parent reports subagent still working (not completed).
	src.AddUpdate("parent-1", source.SourceUpdate{
		SessionID: "parent-1",
		Slug:      "parent-slug",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "worker-slug", Activity: session.ActivityWorking},
		},
	}, "c1")
	src.AddUpdate("sub-1", source.SourceUpdate{
		SessionID: "sub-1",
		Slug:      "worker-slug",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	snap := m.Snapshot()
	for i := 0; i < len(snap); i++ {
		if snap[i].ID == "sub-1" && snap[i].Lifecycle != session.LifecycleActive {
			t.Errorf("sub-1 should stay active when subagent is still working, got %q", snap[i].Lifecycle)
		}
	}
}

// TestPollOnce_SubagentCompletion_DoesNotAffectParent verifies that the
// parent session is never marked terminal by the subagent sweep, even though
// it hosts the terminal subagent state.
func TestPollOnce_SubagentCompletion_DoesNotAffectParent(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))

	src.SetHandles([]source.SessionHandle{
		{ID: "parent", Source: "test", StartedAt: clk.Now()},
		{ID: "child", Source: "test", StartedAt: clk.Now()},
	})

	// Both active.
	src.AddUpdate("parent", source.SourceUpdate{
		SessionID: "parent",
		Slug:      "main-session",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "child-slug", Activity: session.ActivityWorking},
		},
	}, "c1")
	src.AddUpdate("child", source.SourceUpdate{
		SessionID: "child",
		Slug:      "child-slug",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Parent reports subagent done.
	clk.Advance(time.Second)
	src.AddUpdate("parent", source.SourceUpdate{
		SessionID: "parent",
		Slug:      "main-session",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "child-slug", Activity: session.ActivityTerminal},
		},
	}, "c2")

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	snap := m.Snapshot()
	byID := make(map[string]session.SessionState)
	for i := 0; i < len(snap); i++ {
		byID[snap[i].ID] = snap[i]
	}

	if byID["parent"].Lifecycle != session.LifecycleActive {
		t.Errorf("parent should stay active, got %q", byID["parent"].Lifecycle)
	}
	if byID["child"].Lifecycle != session.LifecycleTerminal {
		t.Errorf("child should be terminal, got %q", byID["child"].Lifecycle)
	}
}

// TestRun_Race exercises concurrent Run + Snapshot under the race detector.
func TestRun_Race(t *testing.T) {
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
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	yieldToRun()

	// Concurrently advance clock and take snapshots.
	snapshotDone := make(chan struct{})
	go func() {
		defer close(snapshotDone)
		for i := 0; i < 50; i++ {
			_ = m.Snapshot()
		}
	}()

	for i := 0; i < 10; i++ {
		clk.Advance(time.Second)
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID: "s1",
			Activity:  session.ActivityWorking,
		}, source.Cursor(fmt.Sprintf("c%d", i+2)))
		time.Sleep(time.Millisecond) // let goroutines interleave
	}

	<-snapshotDone
	cancel()
	_ = waitForDone(t, done)
}
