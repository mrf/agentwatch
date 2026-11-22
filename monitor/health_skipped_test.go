package monitor_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

func newSkippedMonitor(t *testing.T, src *mock.Source) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(testClock()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// TestHealth_SkippedLinesAccumulate verifies that SourceUpdate.SkippedLines
// is summed across polls and sessions into Health.SkippedLines without
// affecting status.
func TestHealth_SkippedLinesAccumulate(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("claude"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude"},
		{ID: "s2", Source: "claude"},
	})
	src.AddUpdate("s1", source.SourceUpdate{SessionID: "s1", Activity: session.ActivityWorking, SkippedLines: 2}, "c1")
	src.AddUpdate("s1", source.SourceUpdate{SessionID: "s1", Activity: session.ActivityWorking, SkippedLines: 1}, "c2")
	src.AddUpdate("s2", source.SourceUpdate{SessionID: "s2", Activity: session.ActivityWorking, SkippedLines: 4}, "c1")

	m := newSkippedMonitor(t, src)
	ctx := context.Background()

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	if got := m.Health()["claude"].SkippedLines; got != 6 {
		t.Errorf("SkippedLines after poll 1 = %d, want 6", got)
	}

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}
	h := m.Health()["claude"]
	if h.SkippedLines != 7 {
		t.Errorf("SkippedLines after poll 2 = %d, want 7", h.SkippedLines)
	}
	if h.Status != monitor.HealthHealthy || h.LastError != "" {
		t.Errorf("skipped lines must not affect status: %q / %q", h.Status, h.LastError)
	}
}

// TestHealth_SkippedLinesWithoutSessionID verifies that a source that only
// skipped lines (no session data) still reports the count, because the
// zero-update "no new data" shortcut must not swallow it.
func TestHealth_SkippedLinesWithoutSessionID(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("codex"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "codex"}})
	src.AddUpdate("s1", source.SourceUpdate{SkippedLines: 3}, "c1")

	m := newSkippedMonitor(t, src)
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := m.Health()["codex"].SkippedLines; got != 3 {
		t.Errorf("SkippedLines = %d, want 3", got)
	}
	if n := len(m.Snapshot()); n != 0 {
		t.Errorf("a SessionID-less update must not create a session, got %d", n)
	}
}

func TestHealth_SkippedLinesJSON(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(monitor.Health{Source: "x", SkippedLines: 5})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["skippedLines"] != float64(5) {
		t.Errorf("skippedLines = %v, want 5 (json: %s)", m["skippedLines"], b)
	}
}
