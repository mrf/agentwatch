package monitor_test

import (
	"context"
	"testing"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

func TestSnapshotWithSeq_EmptyMonitorIsSeqZero(t *testing.T) {
	t.Parallel()
	m, err := monitor.New(monitor.WithSources(mock.New(mock.WithName("test"))), monitor.WithClock(testClock()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessions, seq := m.SnapshotWithSeq()
	if len(sessions) != 0 || seq != 0 {
		t.Fatalf("got %d sessions, seq %d; want 0, 0", len(sessions), seq)
	}
}

func TestSnapshotWithSeq_SeqMatchesLastDeliveredEvent(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test", StartedAt: clk.Now()}})
	src.AddUpdate("s1", source.Update{SessionID: "s1", Activity: session.ActivityWorking}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk), monitor.WithSink(sink))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(sink.events) == 0 {
		t.Fatal("expected events from PollOnce")
	}

	sessions, seq := m.SnapshotWithSeq()
	last := sink.events[len(sink.events)-1].Seq
	if seq != last {
		t.Fatalf("snapshot seq = %d, want last event seq %d", seq, last)
	}
	if len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

// State is committed before an event is delivered, so a snapshot taken from
// inside the sink must already reflect the event it carries, and its seq must
// be at least the event's seq.
func TestSnapshotWithSeq_SnapshotInSinkReflectsDeliveredEvent(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test", StartedAt: clk.Now()}})
	src.AddUpdate("s1", source.Update{SessionID: "s1", Activity: session.ActivityWorking}, "c1")

	var m *monitor.Monitor
	checked := 0
	sink := &funcSink{fn: func(_ context.Context, ev monitor.Event) error {
		if ev.Type != monitor.EventDelta {
			return nil
		}
		sessions, seq := m.SnapshotWithSeq()
		if seq < ev.Seq {
			t.Errorf("snapshot seq %d < delta seq %d", seq, ev.Seq)
		}
		if len(sessions) != 1 || sessions[0].Activity != session.ActivityWorking {
			t.Errorf("snapshot does not reflect delta: %+v", sessions)
		}
		checked++
		return nil
	}}
	var err error
	m, err = monitor.New(monitor.WithSources(src), monitor.WithClock(clk), monitor.WithSink(sink))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if checked == 0 {
		t.Fatal("sink never saw a delta")
	}
}
