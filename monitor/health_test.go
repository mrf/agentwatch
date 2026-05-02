package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// --- Health: initial state is healthy ---

func TestHealth_InitiallyHealthy(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("claude"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	health := m.Health()
	if len(health) != 1 {
		t.Fatalf("expected 1 health entry, got %d", len(health))
	}
	h := health["claude"]
	if h.Source != "claude" {
		t.Errorf("Source = %q, want %q", h.Source, "claude")
	}
	if h.Status != monitor.HealthHealthy {
		t.Errorf("Status = %q, want %q", h.Status, monitor.HealthHealthy)
	}
	if h.DiscoverFailures != 0 {
		t.Errorf("DiscoverFailures = %d, want 0", h.DiscoverFailures)
	}
	if h.ParseFailures != 0 {
		t.Errorf("ParseFailures = %d, want 0", h.ParseFailures)
	}
}

// --- Health: healthy to degraded to failed ---

func TestHealth_HealthyToDegradedToFailed(t *testing.T) {
	t.Parallel()
	clk := testClock()
	threshold := 3
	src := mock.New(mock.WithName("claude"))

	// Queue discover errors: threshold failures -> degraded, then more -> failed.
	// First poll succeeds to establish healthy baseline.
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Poll 1: success -> healthy.
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	assertHealthStatus(t, m, "claude", monitor.HealthHealthy)

	// Queue discover errors up to threshold.
	for i := 0; i < threshold; i++ {
		clk.Advance(time.Second)
		src.QueueDiscoverError(fmt.Errorf("discover error %d", i))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i+2, err)
		}
	}
	// At threshold, should be degraded.
	assertHealthStatus(t, m, "claude", monitor.HealthDegraded)

	// Queue more errors to reach 2*threshold -> failed.
	for i := 0; i < threshold; i++ {
		clk.Advance(time.Second)
		src.QueueDiscoverError(fmt.Errorf("discover error %d", threshold+i))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", threshold+i+2, err)
		}
	}
	assertHealthStatus(t, m, "claude", monitor.HealthFailed)

	h := m.Health()["claude"]
	if h.DiscoverFailures != 2*threshold {
		t.Errorf("DiscoverFailures = %d, want %d", h.DiscoverFailures, 2*threshold)
	}
}

// --- Health: parse failures degrade health ---

func TestHealth_ParseFailuresDegradeHealth(t *testing.T) {
	t.Parallel()
	clk := testClock()
	threshold := 2
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// First poll succeeds.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	assertHealthStatus(t, m, "test", monitor.HealthHealthy)

	// Queue parse errors.
	for i := 0; i < threshold; i++ {
		clk.Advance(time.Second)
		src.QueueParseResult("s1", source.SourceUpdate{}, "", fmt.Errorf("parse error %d", i))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i+2, err)
		}
	}
	assertHealthStatus(t, m, "test", monitor.HealthDegraded)

	h := m.Health()["test"]
	if h.ParseFailures != threshold {
		t.Errorf("ParseFailures = %d, want %d", h.ParseFailures, threshold)
	}
}

// --- Health: recovery back to healthy ---

func TestHealth_RecoveryToHealthy(t *testing.T) {
	t.Parallel()
	clk := testClock()
	threshold := 2
	src := mock.New(mock.WithName("claude"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude", StartedAt: clk.Now()},
	})

	// First poll succeeds.
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Push to degraded with discover failures.
	for i := 0; i < threshold; i++ {
		clk.Advance(time.Second)
		src.QueueDiscoverError(fmt.Errorf("error %d", i))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	assertHealthStatus(t, m, "claude", monitor.HealthDegraded)

	// Successful poll clears failures and resets to healthy.
	clk.Advance(time.Second)
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityIdle,
	}, "c2")
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce recovery: %v", err)
	}
	assertHealthStatus(t, m, "claude", monitor.HealthHealthy)

	h := m.Health()["claude"]
	if h.DiscoverFailures != 0 {
		t.Errorf("DiscoverFailures = %d, want 0 after recovery", h.DiscoverFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty after recovery", h.LastError)
	}
}

// --- Health: EventHealth delivered on status change ---

func TestHealth_EventDeliveredOnStatusChange(t *testing.T) {
	t.Parallel()
	clk := testClock()
	threshold := 1
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Initial poll — healthy (initial state, no event expected for initial healthy).
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Clear sink events from initial poll.
	sink.events = nil

	// Degrade with discover error.
	clk.Advance(time.Second)
	src.QueueDiscoverError(errors.New("timeout"))
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	// Should have a health event.
	var healthEvents []monitor.Event
	for i := 0; i < len(sink.events); i++ {
		if sink.events[i].Type == monitor.EventHealth {
			healthEvents = append(healthEvents, sink.events[i])
		}
	}
	if len(healthEvents) != 1 {
		t.Fatalf("expected 1 health event, got %d", len(healthEvents))
	}
	hev := healthEvents[0]
	if hev.Health == nil {
		t.Fatal("Health event has nil Health")
	}
	if hev.Health.Status != monitor.HealthDegraded {
		t.Errorf("Health.Status = %q, want %q", hev.Health.Status, monitor.HealthDegraded)
	}
	if hev.Health.Source != "test" {
		t.Errorf("Health.Source = %q, want %q", hev.Health.Source, "test")
	}
	if hev.Seq == 0 {
		t.Error("Health event Seq should be > 0")
	}
}

// --- Health: no event when status unchanged ---

func TestHealth_NoEventWhenStatusUnchanged(t *testing.T) {
	t.Parallel()
	clk := testClock()
	threshold := 3
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	sink.events = nil

	// One failure — still healthy (below threshold).
	clk.Advance(time.Second)
	src.QueueDiscoverError(errors.New("blip"))
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	for i := 0; i < len(sink.events); i++ {
		if sink.events[i].Type == monitor.EventHealth {
			t.Error("should not emit health event when status unchanged")
		}
	}
}

// --- Health: error sanitization ---

func TestHealth_ErrorSanitization(t *testing.T) {
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

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	tests := []struct {
		name     string
		errMsg   string
		wantLack string // substring that must NOT appear
	}{
		{
			name:     "absolute path redacted",
			errMsg:   "open /home/user/.config/claude/sessions/abc.jsonl: no such file",
			wantLack: "/home/user",
		},
		{
			name:     "windows path redacted",
			errMsg:   `open C:\Users\dev\sessions\abc.jsonl: access denied`,
			wantLack: `C:\Users`,
		},
		{
			name:     "panic details redacted",
			errMsg:   "panic: runtime error: index out of range [5] with length 3\ngoroutine 1 [running]:\nmain.main()\n\t/home/user/project/main.go:42",
			wantLack: "goroutine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk.Advance(time.Second)
			src.QueueDiscoverError(errors.New(tt.errMsg))
			if err := m.PollOnce(ctx); err != nil {
				t.Fatalf("PollOnce: %v", err)
			}

			h := m.Health()["test"]
			if h.LastError == "" {
				t.Fatal("LastError should not be empty after failure")
			}
			if strings.Contains(h.LastError, tt.wantLack) {
				t.Errorf("LastError %q should not contain %q", h.LastError, tt.wantLack)
			}
		})
	}
}

// --- Health: multi-source independent tracking ---

func TestHealth_MultiSourceIndependent(t *testing.T) {
	t.Parallel()
	clk := testClock()

	src1 := mock.New(mock.WithName("claude"))
	src1.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "claude", StartedAt: clk.Now()},
	})
	src1.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	src2 := mock.New(mock.WithName("codex"))
	src2.SetHandles([]source.SessionHandle{
		{ID: "s2", Source: "codex", StartedAt: clk.Now()},
	})
	src2.AddUpdate("s2", source.SourceUpdate{
		SessionID: "s2",
		Activity:  session.ActivityWorking,
	}, "x1")

	m, err := monitor.New(
		monitor.WithSources(src1, src2),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// Fail src1 only.
	clk.Advance(time.Second)
	src1.QueueDiscoverError(errors.New("src1 down"))
	src2.AddUpdate("s2", source.SourceUpdate{
		SessionID: "s2",
		Activity:  session.ActivityIdle,
	}, "x2")

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 2: %v", err)
	}

	health := m.Health()
	if health["claude"].Status != monitor.HealthDegraded {
		t.Errorf("claude Status = %q, want %q", health["claude"].Status, monitor.HealthDegraded)
	}
	if health["codex"].Status != monitor.HealthHealthy {
		t.Errorf("codex Status = %q, want %q", health["codex"].Status, monitor.HealthHealthy)
	}
}

// --- Health: default threshold ---

func TestHealth_DefaultThreshold(t *testing.T) {
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

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		// No WithHealthThreshold — uses default (3).
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}

	// 2 failures: still healthy (below default threshold of 3).
	for i := 0; i < 2; i++ {
		clk.Advance(time.Second)
		src.QueueDiscoverError(fmt.Errorf("error %d", i))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	assertHealthStatus(t, m, "test", monitor.HealthHealthy)

	// 3rd failure: degraded at default threshold.
	clk.Advance(time.Second)
	src.QueueDiscoverError(errors.New("error 2"))
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	assertHealthStatus(t, m, "test", monitor.HealthDegraded)
}

// --- Health: race detector ---

func TestHealth_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	clk := clock.NewMock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
	}, "c1")

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = m.Health()
		}
	}()

	for i := 0; i < 100; i++ {
		_ = m.Snapshot()
	}
	<-done
}

// --- helpers ---

func assertHealthStatus(t *testing.T, m *monitor.Monitor, sourceName string, want monitor.HealthStatus) {
	t.Helper()
	health := m.Health()
	h, ok := health[sourceName]
	if !ok {
		t.Fatalf("no health entry for source %q", sourceName)
	}
	if h.Status != want {
		t.Errorf("health[%q].Status = %q, want %q", sourceName, h.Status, want)
	}
}

