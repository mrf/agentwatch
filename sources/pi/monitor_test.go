package pi_test

import (
	"context"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/pi"
)

// TestMonitorRecoversFromCorruptCursor: a cursor damaged between polls must
// make the source re-read the file as a Reset, so the monitor replaces the
// counts instead of adding the whole file a second time.
func TestMonitorRecoversFromCorruptCursor(t *testing.T) {
	root, _ := writeSession(t, "full001", fullSessionFixture)
	src := &testutil.CorruptingSource{Source: pi.New(pi.WithRoot(root))}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clock.NewFake(time.Date(2024, 1, 15, 10, 1, 0, 0, time.UTC))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].MessageCount == 0 {
		t.Fatalf("first poll: snapshot = %+v, want one session with messages", snap)
	}
	want := snap[0]

	src.SetCorrupt(true)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	src.SetCorrupt(false)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap = m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d sessions, want 1", len(snap))
	}
	if snap[0].MessageCount != want.MessageCount || snap[0].ToolCallCount != want.ToolCallCount {
		t.Errorf("after corrupt cursor: %d msgs/%d tools, want %d/%d",
			snap[0].MessageCount, snap[0].ToolCallCount, want.MessageCount, want.ToolCallCount)
	}
}
