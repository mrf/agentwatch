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

// TestSnapshot_SortedBySourceThenID verifies Snapshot order is deterministic
// regardless of map iteration order or discovery order.
func TestSnapshot_SortedBySourceThenID(t *testing.T) {
	t.Parallel()
	clk := testClock()
	srcB := mock.New(mock.WithName("b"))
	srcA := mock.New(mock.WithName("a"))
	ids := []string{"s3", "s1", "s5", "s2", "s4", "s0"}
	names := []string{"b", "a"}
	for n, src := range []*mock.Source{srcB, srcA} {
		handles := make([]source.SessionHandle, 0, len(ids))
		for i := 0; i < len(ids); i++ {
			id := names[n] + "-" + ids[i]
			handles = append(handles, source.SessionHandle{ID: id, Source: names[n], StartedAt: clk.Now()})
			src.AddUpdate(id, source.SourceUpdate{SessionID: id, Activity: session.ActivityWorking}, "c1")
		}
		src.SetHandles(handles)
	}

	m, err := monitor.New(monitor.WithSources(srcB, srcA), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	for n := 0; n < 20; n++ {
		snap, _ := m.SnapshotWithSeq()
		if len(snap) != 12 {
			t.Fatalf("expected 12 sessions, got %d", len(snap))
		}
		for i := 1; i < len(snap); i++ {
			prev, cur := snap[i-1], snap[i]
			if prev.Source > cur.Source || (prev.Source == cur.Source && prev.ID >= cur.ID) {
				t.Fatalf("snapshot not sorted by (Source, ID) at %d: %s/%s before %s/%s",
					i, prev.Source, prev.ID, cur.Source, cur.ID)
			}
		}
	}
}

// TestPollOnce_DeltaHasAtMostOneUpdatePerSession verifies a session updated in
// the parse loop and then terminated by a sweep in the same poll appears once
// in Event.Updates, carrying its final (terminal) state.
func TestPollOnce_DeltaHasAtMostOneUpdatePerSession(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "parent", Source: "test", StartedAt: clk.Now()},
		{ID: "child", Source: "test", StartedAt: clk.Now()},
	})
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

	sink := &recordingSink{}
	m, err := monitor.New(monitor.WithSources(src), monitor.WithSink(sink), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Same poll: parent reports the subagent done AND the child gets new data,
	// so the child is updated by the parse loop then terminated by the sweep.
	clk.Advance(time.Second)
	src.AddUpdate("parent", source.SourceUpdate{
		SessionID: "parent",
		Slug:      "main-session",
		Activity:  session.ActivityWorking,
		Subagents: []session.SubagentState{
			{ID: "tu-1", Slug: "child-slug", Activity: session.ActivityTerminal},
		},
	}, "c2")
	src.AddUpdate("child", source.SourceUpdate{
		SessionID:     "child",
		Slug:          "child-slug",
		Activity:      session.ActivityWorking,
		ContextTokens: 42,
	}, "c2")
	sink.events = nil
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	var deltas int
	for i := 0; i < len(sink.events); i++ {
		ev := sink.events[i]
		if ev.Type != monitor.EventDelta {
			continue
		}
		deltas++
		seen := make(map[string]session.SessionState)
		for j := 0; j < len(ev.Updates); j++ {
			u := ev.Updates[j]
			if _, dup := seen[u.ID]; dup {
				t.Errorf("session %q appears more than once in delta Updates", u.ID)
			}
			seen[u.ID] = u
		}
		child, ok := seen["child"]
		if !ok {
			t.Fatal("child missing from delta Updates")
		}
		if child.Lifecycle != session.LifecycleTerminal {
			t.Errorf("child update Lifecycle = %q, want last state %q", child.Lifecycle, session.LifecycleTerminal)
		}
	}
	if deltas != 1 {
		t.Fatalf("expected 1 delta event, got %d", deltas)
	}
}
