package cursor_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/cursor"
)

// TestMonitorRecoversFromCorruptCursor: a cursor damaged between polls must
// make the source re-read the file as a Reset, so the monitor replaces the
// counts instead of adding the whole file a second time.
func TestMonitorRecoversFromCorruptCursor(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "sess001", flatFixture)
	// <root>/projects/<project>/agent-transcripts/<id>.jsonl
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(h.Path))))
	src := &testutil.CorruptingSource{Source: cursor.New(cursor.WithRoot(root))}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clock.NewFake(time.Now())),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].MessageCount != 2 {
		t.Fatalf("first poll: snapshot = %+v, want one session with 2 messages", snap)
	}

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
	if snap[0].MessageCount != 2 {
		t.Errorf("MessageCount after corrupt cursor = %d, want 2", snap[0].MessageCount)
	}
}
