package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// removedSessionFixture drives s1 through discover (3 messages, 2 tool
// calls, started at t0) -> terminal -> removed, leaving the monitor ready for
// a resume poll.
func removedSessionFixture(t *testing.T) (*monitor.Monitor, *mock.Source, *recordingSink, time.Time) {
	t.Helper()
	clk := testClock()
	t0 := clk.Now()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: t0.Add(time.Hour)},
	})
	src.QueueParseResult("s1", source.Update{
		SessionID:          "s1",
		StartedAt:          t0,
		MessageCountDelta:  3,
		ToolCallCountDelta: 2,
	}, "c1", nil)
	src.QueueParseResult("s1", source.Update{
		SessionID: "s1",
		Terminal:  true,
	}, "c2", nil)

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
		t.Fatalf("PollOnce discover: %v", err)
	}
	clk.Advance(time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce terminal: %v", err)
	}
	clk.Advance(retention + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce removal: %v", err)
	}
	if n := len(m.Snapshot()); n != 0 {
		t.Fatalf("expected session removed, got %d in snapshot", n)
	}
	sink.events = nil
	return m, src, sink, t0
}

// A removed session that resumes must report whole-history counts, not just
// the counts from the batch that revived it (agentwatch-l72).
func TestResumeAfterRemoval_CountsCoverFullHistory(t *testing.T) {
	t.Parallel()
	m, src, sink, t0 := removedSessionFixture(t)

	// Resume poll: the incremental parse (cursor c2) sees one new message;
	// the monitor then re-reads from a zero cursor and gets the full history.
	src.QueueParseResult("s1", source.Update{
		SessionID:         "s1",
		StartedAt:         t0.Add(10 * time.Second),
		MessageCountDelta: 1,
	}, "c3", nil)
	src.QueueParseResult("s1", source.Update{
		SessionID:          "s1",
		StartedAt:          t0,
		MessageCountDelta:  4,
		ToolCallCountDelta: 2,
	}, "c4", nil)

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce resume: %v", err)
	}

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session after resume, got %d", len(snap))
	}
	got := snap[0]
	if got.MessageCount != 4 {
		t.Errorf("MessageCount = %d, want 4 (full history)", got.MessageCount)
	}
	if got.ToolCallCount != 2 {
		t.Errorf("ToolCallCount = %d, want 2 (full history)", got.ToolCallCount)
	}
	if !got.StartedAt.Equal(t0) {
		t.Errorf("StartedAt = %v, want earliest content timestamp %v", got.StartedAt, t0)
	}
	if got.Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q, want active", got.Lifecycle)
	}

	ev, ok := findLifecycleEvent(sink.events, session.EventResumed)
	if !ok {
		t.Fatal("no resumed event")
	}
	if ev.Lifecycle.From != session.LifecycleTerminal || ev.Lifecycle.To != session.LifecycleActive {
		t.Errorf("resumed %q -> %q, want terminal -> active", ev.Lifecycle.From, ev.Lifecycle.To)
	}

	// The full re-read starts from a zero cursor; its cursor is the one kept.
	calls := src.ParseCalls()
	last := calls[len(calls)-1]
	if last.Cursor != "" {
		t.Errorf("re-read cursor = %q, want zero cursor", last.Cursor)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce after resume: %v", err)
	}
	calls = src.ParseCalls()
	if c := calls[len(calls)-1].Cursor; c != "c4" {
		t.Errorf("next poll cursor = %q, want c4", c)
	}
}

// With no new data a removed session stays removed and costs no re-read.
func TestResumeAfterRemoval_NoNewDataNoReread(t *testing.T) {
	t.Parallel()
	m, src, _, _ := removedSessionFixture(t)
	before := src.ParseCallCount()

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if n := len(m.Snapshot()); n != 0 {
		t.Errorf("snapshot has %d sessions, want 0", n)
	}
	if n := src.ParseCallCount() - before; n != 1 {
		t.Errorf("Parse called %d times, want 1 (no re-read)", n)
	}
}

// A failed re-read must not lose the revived batch: the next poll retries
// from the pre-resume cursor and resumes the session.
func TestResumeAfterRemoval_RereadErrorRetries(t *testing.T) {
	t.Parallel()
	m, src, _, t0 := removedSessionFixture(t)

	src.QueueParseResult("s1", source.Update{
		SessionID: "s1", MessageCountDelta: 1,
	}, "c3", nil)
	src.QueueParseResult("s1", source.Update{}, "", errors.New("boom"))

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce failing re-read: %v", err)
	}
	if n := len(m.Snapshot()); n != 0 {
		t.Fatalf("session resumed despite failed re-read (%d in snapshot)", n)
	}

	src.QueueParseResult("s1", source.Update{
		SessionID: "s1", MessageCountDelta: 1,
	}, "c3", nil)
	src.QueueParseResult("s1", source.Update{
		SessionID: "s1", StartedAt: t0, MessageCountDelta: 4, ToolCallCountDelta: 2,
	}, "c4", nil)
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce retry: %v", err)
	}

	calls := src.ParseCalls()
	if c := calls[len(calls)-2].Cursor; c != "c2" {
		t.Errorf("retry incremental cursor = %q, want c2 (pre-resume cursor)", c)
	}
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].MessageCount != 4 {
		t.Fatalf("after retry snapshot = %+v, want 1 session with 4 messages", snap)
	}
}
