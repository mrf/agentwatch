package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

const boundedRetention = 5 * time.Second

func newBoundedMonitor(t *testing.T, src *mock.Source, sink *recordingSink) (*monitor.Monitor, func()) {
	t.Helper()
	clk := testClock()
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithCompletionRetention(boundedRetention),
		monitor.WithStaleThreshold(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	poll := func() {
		t.Helper()
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		clk.Advance(boundedRetention + time.Second)
	}
	return m, poll
}

// Sessions that come, end, get removed, and stop being discovered must not
// leave cursor or removed-ID entries behind forever.
func TestPollOnce_ChurnKeepsBookkeepingBounded(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	m, poll := newBoundedMonitor(t, src, &recordingSink{})

	const sessions = 200
	const bound = 4
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("s%d", i)
		src.SetHandles([]source.SessionHandle{{ID: id, Source: "test"}})
		src.QueueParseResult(id, source.SourceUpdate{SessionID: id, MessageCountDelta: 1}, "c1", nil)
		src.QueueParseResult(id, source.SourceUpdate{SessionID: id, Terminal: true}, "c2", nil)

		poll() // discovered
		poll() // terminal
		poll() // removed

		if c, r := monitor.TrackedSizes(m); c > bound || r > bound {
			t.Fatalf("after %d sessions: cursors=%d removed=%d, want both <= %d", i+1, c, r, bound)
		}
	}

	src.SetHandles(nil)
	poll()
	poll()
	if c, r := monitor.TrackedSizes(m); c != 0 || r != 0 {
		t.Fatalf("after churn settles: cursors=%d removed=%d, want 0/0", c, r)
	}
}

// removeAndForget drives s1 through discover (3 messages) -> terminal ->
// removed -> undiscovered past the retention window.
func removeAndForget(t *testing.T, src *mock.Source, poll func()) {
	t.Helper()
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", MessageCountDelta: 3}, "c1", nil)
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", Terminal: true}, "c2", nil)
	poll()
	poll()
	poll()
	src.SetHandles(nil)
	poll()
	poll()
}

// A forgotten session that reappears is re-read from the start and treated
// as new: whole-history counts exactly once, one Discovered event.
func TestPollOnce_ForgottenSessionReappearsWithoutDoubleCounting(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	sink := &recordingSink{}
	m, poll := newBoundedMonitor(t, src, sink)
	removeAndForget(t, src, poll)
	if c, r := monitor.TrackedSizes(m); c != 0 || r != 0 {
		t.Fatalf("before reappearance: cursors=%d removed=%d, want 0/0", c, r)
	}

	sink.events = nil
	before := src.ParseCallCount()
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	// Full re-read from a zero cursor: the 3 old messages plus 1 new one.
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", MessageCountDelta: 4}, "c3", nil)
	poll()

	calls := src.ParseCalls()[before:]
	if len(calls) != 1 || calls[0].Cursor != "" {
		t.Fatalf("parse calls on reappearance = %+v, want one call with empty cursor", calls)
	}
	got, ok := m.Get("s1")
	if !ok {
		t.Fatal("s1 missing after reappearance")
	}
	if got.MessageCount != 4 || got.Lifecycle != session.LifecycleActive {
		t.Fatalf("s1 = count %d lifecycle %q, want 4 active", got.MessageCount, got.Lifecycle)
	}
	if _, ok := findLifecycleEvent(sink.events, session.EventDiscovered); !ok {
		t.Fatalf("no Discovered event on reappearance: %+v", sink.events)
	}
	if _, ok := findLifecycleEvent(sink.events, session.EventResumed); ok {
		t.Fatal("unexpected Resumed event for a forgotten session")
	}
}

// A failing Discover says nothing about which sessions still exist, so it
// must not age out their cursors.
func TestPollOnce_DiscoverFailureKeepsCursors(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	m, poll := newBoundedMonitor(t, src, &recordingSink{})

	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", MessageCountDelta: 3}, "c1", nil)
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", Terminal: true}, "c2", nil)
	poll()
	poll()
	poll()

	for i := 0; i < 5; i++ {
		src.QueueDiscoverError(errors.New("walk failed"))
		poll()
	}
	if c, r := monitor.TrackedSizes(m); c != 1 || r != 1 {
		t.Fatalf("after discover failures: cursors=%d removed=%d, want 1/1", c, r)
	}

	before := src.ParseCallCount()
	poll()
	calls := src.ParseCalls()[before:]
	if len(calls) != 1 || calls[0].Cursor != "c2" {
		t.Fatalf("parse calls after recovery = %+v, want one call with cursor c2", calls)
	}
}

// A session still in the store keeps its cursor even when its source stops
// reporting it, so its next batch stays incremental.
func TestPollOnce_StoredSessionKeepsCursorWhenUndiscovered(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	m, poll := newBoundedMonitor(t, src, &recordingSink{})

	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	src.QueueParseResult("s1", source.SourceUpdate{SessionID: "s1", MessageCountDelta: 3}, "c1", nil)
	poll()
	src.SetHandles(nil)
	poll()
	poll()
	if c, _ := monitor.TrackedSizes(m); c != 1 {
		t.Fatalf("cursors=%d, want 1 for a stored session", c)
	}

	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	before := src.ParseCallCount()
	poll()
	calls := src.ParseCalls()[before:]
	if len(calls) != 1 || calls[0].Cursor != "c1" {
		t.Fatalf("parse calls on rediscovery = %+v, want one call with cursor c1", calls)
	}
}
