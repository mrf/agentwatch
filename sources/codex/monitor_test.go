package codex_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/sources/codex"
)

const (
	liveSessionID = "01234567-abcd-ef01-2345-67890abcdef0"

	liveBatch1 = `{"timestamp":"2026-11-09T10:00:00.000Z","type":"session_meta","payload":{"id":"01234567-abcd-ef01-2345-67890abcdef0","timestamp":"2026-11-09T10:00:00.000Z"}}
{"timestamp":"2026-11-09T10:00:01.000Z","type":"event_msg","payload":{"type":"user_message"}}
`
	liveBatch2 = `{"timestamp":"2026-11-09T10:00:30.000Z","type":"event_msg","payload":{"type":"agent_message"}}
{"timestamp":"2026-11-09T10:00:31.000Z","type":"response_item","payload":{"type":"function_call","name":"shell"}}
`
	liveBatch3 = `{"timestamp":"2026-11-09T10:04:00.000Z","type":"event_msg","payload":{"type":"task_complete"}}
`
)

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func onlySession(t *testing.T, m *monitor.Monitor) session.SessionState {
	t.Helper()
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d sessions, want 1", len(snap))
	}
	return snap[0]
}

// TestMonitorAppliesLaterBatches drives a rollout through a real monitor in
// several batches. Only the first batch carries session_meta; later batches
// must still be applied, keep the session alive, and populate LastActivityAt.
func TestMonitorAppliesLaterBatches(t *testing.T) {
	root, h := writeRollout(t, liveSessionID, liveBatch1)
	clk := clock.NewFake(time.Date(2026, 11, 9, 10, 0, 5, 0, time.UTC))
	m, err := monitor.New(
		monitor.WithSources(codex.New(codex.WithRoot(root))),
		monitor.WithClock(clk),
		monitor.WithStaleThreshold(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s := onlySession(t, m)
	assertInt(t, "batch1 MessageCount", s.MessageCount, 1)
	wantStart := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if !s.StartedAt.Equal(wantStart) {
		t.Errorf("StartedAt = %v, want %v", s.StartedAt, wantStart)
	}
	wantAct1 := time.Date(2026, 11, 9, 10, 0, 1, 0, time.UTC)
	if !s.LastActivityAt.Equal(wantAct1) {
		t.Errorf("batch1 LastActivityAt = %v, want %v", s.LastActivityAt, wantAct1)
	}

	// Batch 2: no session_meta.
	appendFile(t, h.Path, liveBatch2)
	clk.Advance(4 * time.Minute)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s = onlySession(t, m)
	assertInt(t, "batch2 MessageCount", s.MessageCount, 2)
	assertInt(t, "batch2 ToolCallCount", s.ToolCallCount, 1)
	if s.Activity != session.ActivityWorking {
		t.Errorf("batch2 Activity = %q, want working", s.Activity)
	}
	wantAct2 := time.Date(2026, 11, 9, 10, 0, 31, 0, time.UTC)
	if !s.LastActivityAt.Equal(wantAct2) {
		t.Errorf("batch2 LastActivityAt = %v, want %v", s.LastActivityAt, wantAct2)
	}
	// StartedAt comes from session_meta only; later envelopes must not move it.
	if !s.StartedAt.Equal(wantStart) {
		t.Errorf("batch2 StartedAt = %v, want %v", s.StartedAt, wantStart)
	}

	// Batch 3 lands 4m after batch 2: 8m since the first poll, past the 5m
	// stale threshold, but the session kept receiving appends.
	appendFile(t, h.Path, liveBatch3)
	clk.Advance(4 * time.Minute)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s = onlySession(t, m)
	if s.Lifecycle != session.LifecycleActive {
		t.Errorf("Lifecycle = %q after live appends, want active", s.Lifecycle)
	}
	if s.Activity != session.ActivityWaiting {
		t.Errorf("task_complete Activity = %q, want waiting", s.Activity)
	}

	// Idle polls (no new bytes) must not refresh LastDataReceivedAt, so a
	// genuinely quiet session still goes stale.
	clk.Advance(6 * time.Minute)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s = onlySession(t, m)
	if s.Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q after quiet period, want terminal", s.Lifecycle)
	}
}
