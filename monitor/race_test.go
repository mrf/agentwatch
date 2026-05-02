package monitor_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// TestRace_ConcurrentSnapshotDuringPollOnce verifies that calling Snapshot()
// from multiple goroutines while PollOnce() processes updates does not race.
func TestRace_ConcurrentSnapshotDuringPollOnce(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
		{ID: "s2", Source: "test", StartedAt: clk.Now()},
		{ID: "s3", Source: "test", StartedAt: clk.Now()},
	})

	const iterations = 50
	for i := 0; i < iterations; i++ {
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID:         "s1",
			Activity:          session.ActivityWorking,
			MessageCountDelta: 1,
		}, source.Cursor(fmt.Sprintf("c1-%d", i)))
		src.AddUpdate("s2", source.SourceUpdate{
			SessionID:          "s2",
			Activity:           session.ActivityIdle,
			ToolCallCountDelta: 1,
		}, source.Cursor(fmt.Sprintf("c2-%d", i)))
		src.AddUpdate("s3", source.SourceUpdate{
			SessionID:     "s3",
			Activity:      session.ActivityWorking,
			ContextTokens: i * 100,
		}, source.Cursor(fmt.Sprintf("c3-%d", i)))
	}

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	const readers = 5
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := m.Snapshot()
					_ = len(snap)
				}
			}
		}()
	}

	for i := 0; i < iterations; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
		clk.Advance(time.Second)
	}

	close(stop)
	wg.Wait()

	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Errorf("final snapshot has %d sessions, want 3", len(snap))
	}
}

// TestRace_ConcurrentSnapshotDuringRepeatedPolling simulates a Run loop by
// calling PollOnce from one goroutine while multiple readers call Snapshot()
// and Health(). This catches races that only manifest under sustained polling.
func TestRace_ConcurrentSnapshotDuringRepeatedPolling(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	const iterations = 100
	for i := 0; i < iterations; i++ {
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID:         "s1",
			Activity:          session.ActivityWorking,
			MessageCountDelta: 1,
		}, source.Cursor(fmt.Sprintf("c-%d", i)))
	}

	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Poller goroutine (simulates Run).
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		for i := 0; i < iterations; i++ {
			_ = m.PollOnce(ctx)
			clk.Advance(time.Second)
		}
	}()

	// Reader goroutines call Snapshot and Health concurrently with polling.
	const readers = 3
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-pollDone:
					return
				default:
					_ = m.Snapshot()
					_ = m.Health()
				}
			}
		}()
	}

	<-pollDone
	wg.Wait()
}

// TestRace_ConcurrentPollOnce runs PollOnce from multiple goroutines
// simultaneously. PollOnce does not reject concurrent calls — the internal
// mutex serializes individual state updates — so this verifies no data races.
func TestRace_ConcurrentPollOnce(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	// Queue enough updates for all concurrent callers.
	const goroutines = 10
	const pollsPerGoroutine = 20
	for i := 0; i < goroutines*pollsPerGoroutine; i++ {
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID:         "s1",
			Activity:          session.ActivityWorking,
			MessageCountDelta: 1,
		}, source.Cursor(fmt.Sprintf("c-%d", i)))
	}

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := 0; p < pollsPerGoroutine; p++ {
				_ = m.PollOnce(ctx)
			}
		}()
	}

	wg.Wait()

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	// Each consumed update adds 1 message. Not all updates may be consumed
	// (race between goroutines for the queue) but at least some must be.
	if snap[0].MessageCount == 0 {
		t.Error("MessageCount should be > 0 after concurrent polling")
	}
}

// TestRace_SinkDeliveryOutsideStoreLock is a deadlock detection test ported
// from agent-racer. The sink re-enters the monitor by calling Snapshot() and
// Health() from inside HandleEvent. If the store lock were held during sink
// delivery, Snapshot() would deadlock. This is a more aggressive variant of
// TestPollOnce_StateCommittedBeforeSinkCall: it uses multiple sessions,
// verifies all lock-acquiring methods, and counts re-entrant calls.
func TestRace_SinkDeliveryOutsideStoreLock(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
		{ID: "s2", Source: "test", StartedAt: clk.Now()},
	})
	src.AddUpdate("s1", source.SourceUpdate{
		SessionID: "s1",
		Activity:  session.ActivityWorking,
		Model:     "opus",
	}, "c1")
	src.AddUpdate("s2", source.SourceUpdate{
		SessionID: "s2",
		Activity:  session.ActivityIdle,
		Model:     "sonnet",
	}, "c1")

	var mon *monitor.Monitor
	var snapshotCount atomic.Int32
	var healthCount atomic.Int32

	sink := &funcSink{fn: func(_ context.Context, ev monitor.Event) error {
		// Re-enter the monitor from inside the sink callback.
		// If the lock is held during delivery, this deadlocks.
		snap := mon.Snapshot()
		snapshotCount.Add(1)
		_ = len(snap)

		health := mon.Health()
		healthCount.Add(1)
		_ = len(health)

		return nil
	}}

	var err error
	mon, err = monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if snapshotCount.Load() == 0 {
		t.Error("sink was never called (snapshot re-entry count = 0)")
	}
	if healthCount.Load() == 0 {
		t.Error("sink was never called (health re-entry count = 0)")
	}

	snap := mon.Snapshot()
	if len(snap) != 2 {
		t.Errorf("final snapshot has %d sessions, want 2", len(snap))
	}
}

// TestRace_SlowSinkDoesNotBlockSnapshot verifies that a slow (blocking) sink
// does not prevent Snapshot() from completing. This proves the §12.8 invariant
// temporally: the store lock is released before event delivery begins.
//
// The channel-based gate provides deterministic synchronization. The time.After
// is a safety net for deadlock detection only — it never fires on correct code.
func TestRace_SlowSinkDoesNotBlockSnapshot(t *testing.T) {
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

	var mon *monitor.Monitor

	sinkEntered := make(chan struct{}, 1)
	gate := make(chan struct{})

	sink := &funcSink{fn: func(_ context.Context, ev monitor.Event) error {
		select {
		case sinkEntered <- struct{}{}:
		default:
		}
		<-gate
		return nil
	}}

	var err error
	mon, err = monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// PollOnce will block inside the sink after committing state.
	pollDone := make(chan error, 1)
	go func() {
		pollDone <- mon.PollOnce(ctx)
	}()

	// Wait for the sink to be entered.
	<-sinkEntered

	// Snapshot must complete even though the sink is blocked.
	// If the lock were held during delivery, this would deadlock.
	snapshotDone := make(chan []session.SessionState, 1)
	go func() {
		snapshotDone <- mon.Snapshot()
	}()

	select {
	case snap := <-snapshotDone:
		if len(snap) != 1 {
			t.Errorf("snapshot has %d sessions, want 1", len(snap))
		}
		if snap[0].ID != "s1" {
			t.Errorf("session ID = %q, want %q", snap[0].ID, "s1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot() blocked while sink was blocking — lock held during delivery (§12.8 violation)")
	}

	// Release the sink so PollOnce completes.
	close(gate)

	select {
	case err := <-pollDone:
		if err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PollOnce did not complete after sink was released")
	}
}

// TestRace_ConcurrentHealthSnapshotPollOnce runs Health(), Snapshot(), and
// PollOnce() concurrently from separate goroutines. This is the most aggressive
// race test — all three public read/write paths contend on the same mutex.
func TestRace_ConcurrentHealthSnapshotPollOnce(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})

	const iterations = 100
	for i := 0; i < iterations; i++ {
		src.AddUpdate("s1", source.SourceUpdate{
			SessionID:         "s1",
			Activity:          session.ActivityWorking,
			MessageCountDelta: 1,
		}, source.Cursor(fmt.Sprintf("c-%d", i)))
	}

	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(2),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Snapshot reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = m.Snapshot()
			}
		}
	}()

	// Health reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = m.Health()
			}
		}
	}()

	// Poll from the main test goroutine.
	for i := 0; i < iterations; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
		clk.Advance(time.Second)
	}

	close(stop)
	wg.Wait()
}
