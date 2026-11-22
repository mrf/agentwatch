package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// A Terminal=true update for an already-terminal session (e.g. a Claude
// SessionEnd marker consumed after the stale sweep) must not resume it and
// must emit no lifecycle event (agentwatch-r00).
func TestTerminalUpdateOnTerminalSession_DoesNotResume(t *testing.T) {
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
	clk.Advance(staleThreshold + time.Second)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}
	before := m.Snapshot()
	if len(before) != 1 || before[0].Lifecycle != session.LifecycleTerminal {
		t.Fatalf("expected 1 stale-terminal session, got %v", before)
	}

	src.AddUpdate("s1", source.SourceUpdate{SessionID: "s1", Terminal: true}, "c2")
	sink.events = nil
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 3: %v", err)
	}

	after := m.Snapshot()
	if len(after) != 1 {
		t.Fatalf("expected 1 session, got %d", len(after))
	}
	if after[0].Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want %q", after[0].Lifecycle, session.LifecycleTerminal)
	}
	if after[0].CompletedAt == nil || before[0].CompletedAt == nil ||
		!after[0].CompletedAt.Equal(*before[0].CompletedAt) {
		t.Errorf("CompletedAt changed: before=%v after=%v", before[0].CompletedAt, after[0].CompletedAt)
	}
	for i := 0; i < len(sink.events); i++ {
		if ev := sink.events[i]; ev.Type == monitor.EventLifecycle {
			t.Errorf("unexpected lifecycle event %+v", ev.Lifecycle)
		}
	}
}
