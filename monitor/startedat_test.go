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

// pollStartedAt runs one poll per batch against a session whose Discover
// handle carries handleStart, and returns StartedAt after every poll.
func pollStartedAt(t *testing.T, handleStart time.Time, batches []time.Time) []time.Time {
	t.Helper()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: handleStart},
	})
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := make([]time.Time, 0, len(batches))
	for i := 0; i < len(batches); i++ {
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID:         "s1",
			Activity:          session.ActivityWorking,
			MessageCountDelta: 1,
			StartedAt:         batches[i],
		}, source.Cursor(string(rune('a'+i))))
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
		clk.Advance(time.Second)
		snap := m.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("poll %d: want 1 session, got %d", i, len(snap))
		}
		got = append(got, snap[0].StartedAt)
	}
	return got
}

func TestPollOnce_StartedAtKeepsEarliestAcrossBatches(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	// Discover-time mtime is later than every record, as it normally is.
	got := pollStartedAt(t, t0.Add(time.Hour), []time.Time{
		t0, t0.Add(10 * time.Minute), t0.Add(20 * time.Minute),
	})
	for i := 0; i < len(got); i++ {
		if !got[i].Equal(t0) {
			t.Errorf("poll %d: StartedAt = %v, want %v", i, got[i], t0)
		}
	}
}

func TestPollOnce_StartedAtContentOutranksEarlierDiscoverTime(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	first := t0.Add(5 * time.Minute)
	// The handle time predates the first record (file created before it was
	// written); the content timestamp must replace it, then stick.
	got := pollStartedAt(t, t0, []time.Time{first, first.Add(10 * time.Minute)})
	for i := 0; i < len(got); i++ {
		if !got[i].Equal(first) {
			t.Errorf("poll %d: StartedAt = %v, want %v", i, got[i], first)
		}
	}
}

func TestPollOnce_StartedAtEarlierContentLaterWins(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	got := pollStartedAt(t, t0, []time.Time{t0.Add(10 * time.Minute), t0.Add(2 * time.Minute)})
	if want := t0.Add(2 * time.Minute); !got[1].Equal(want) {
		t.Errorf("StartedAt = %v, want %v", got[1], want)
	}
}

func TestPollOnce_StartedAtZeroUpdateKeepsDiscoverTime(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	got := pollStartedAt(t, t0, []time.Time{{}, {}})
	for i := 0; i < len(got); i++ {
		if !got[i].Equal(t0) {
			t.Errorf("poll %d: StartedAt = %v, want %v", i, got[i], t0)
		}
	}
}
