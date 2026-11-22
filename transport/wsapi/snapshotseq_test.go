package wsapi_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// forwardSink lets a monitor be built before the wsapi.Server that is its sink.
type forwardSink struct{ target monitor.EventSink }

func (f *forwardSink) HandleEvent(ctx context.Context, ev monitor.Event) error {
	return f.target.HandleEvent(ctx, ev)
}

// newSinkedServer wires monitor -> wsapi.Server as its sink and runs one poll
// so the monitor's seq is non-zero. It returns the seq of the last event.
func newSinkedServer(t *testing.T) (*wsapi.Server, *httptest.Server, uint64) {
	t.Helper()
	src := mock.New(mock.WithName("test-source"))
	src.SetHandles([]source.SessionHandle{{ID: "sess-1", Source: "test-source"}})
	src.AddUpdate("sess-1", source.SourceUpdate{SessionID: "sess-1", Activity: session.ActivityWorking}, "c1")

	fwd := &forwardSink{}
	mon, err := monitor.New(monitor.WithSources(src), monitor.WithSink(fwd))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv, err := wsapi.NewServer(mon)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	fwd.target = srv

	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	_, seq := mon.SnapshotWithSeq()
	if seq == 0 {
		t.Fatal("expected non-zero monitor seq after PollOnce")
	}

	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Stop()
		ts.Close()
	})
	return srv, ts, seq
}

func TestServer_SnapshotCarriesMonitorSeq(t *testing.T) {
	_, ts, wantSeq := newSinkedServer(t)

	ev := readEvent(t, dialWS(t, ts))
	if ev.Type != monitor.EventSnapshot {
		t.Fatalf("expected snapshot, got %s", ev.Type)
	}
	if ev.Seq != wantSeq {
		t.Fatalf("snapshot seq = %d, want %d", ev.Seq, wantSeq)
	}
}

// A delta enqueued after the client is registered but before the snapshot is
// built must still arrive after the snapshot, and carry a seq above it.
func TestServer_DeltaDuringConnectArrivesAfterSnapshot(t *testing.T) {
	srv, ts, snapSeq := newSinkedServer(t)

	deltaSeq := snapSeq + 1
	wsapi.SetAfterRegisterHook(srv, func() {
		err := srv.HandleEvent(context.Background(), monitor.Event{
			Seq:     deltaSeq,
			At:      time.Now(),
			Type:    monitor.EventDelta,
			Updates: []session.SessionState{{ID: "sess-1", Activity: session.ActivityIdle}},
		})
		if err != nil {
			t.Errorf("HandleEvent: %v", err)
		}
	})

	conn := dialWS(t, ts)

	first := readEvent(t, conn)
	if first.Type != monitor.EventSnapshot {
		t.Fatalf("first message = %s, want snapshot", first.Type)
	}
	second := readEvent(t, conn)
	if second.Type != monitor.EventDelta {
		t.Fatalf("second message = %s, want the concurrent delta", second.Type)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("delta seq %d not above snapshot seq %d", second.Seq, first.Seq)
	}
}
