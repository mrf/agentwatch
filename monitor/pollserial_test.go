package monitor_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// cursorSource hands out the same one-message update to every caller that
// presents the initial (empty) cursor and a zero update once the cursor has
// advanced. Parse sleeps so overlapping polls would both read the empty
// cursor if PollOnce did not serialize them.
type cursorSource struct {
	handle     source.SessionHandle
	parseDelay time.Duration
	parses     atomic.Int32
	entered    chan struct{} // receives once per Parse entry when non-nil
	release    chan struct{} // Parse blocks until closed when non-nil
}

func (c *cursorSource) Name() string { return "cursor" }

func (c *cursorSource) Discover(context.Context) ([]source.SessionHandle, error) {
	return []source.SessionHandle{c.handle}, nil
}

func (c *cursorSource) Parse(_ context.Context, h source.SessionHandle, cur source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	c.parses.Add(1)
	if c.entered != nil {
		c.entered <- struct{}{}
	}
	if c.release != nil {
		<-c.release
	}
	time.Sleep(c.parseDelay)
	if cur != "" {
		return source.SourceUpdate{}, cur, nil
	}
	return source.SourceUpdate{
		SessionID:          h.ID,
		Activity:           session.ActivityWorking,
		MessageCountDelta:  1,
		ToolCallCountDelta: 1,
	}, "done", nil
}

func newCursorSource(now time.Time) *cursorSource {
	return &cursorSource{
		handle:     source.SessionHandle{ID: "s1", Source: "cursor", StartedAt: now},
		parseDelay: 20 * time.Millisecond,
	}
}

func assertSingleApply(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	if snap[0].MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1 (delta double-applied)", snap[0].MessageCount)
	}
	if snap[0].ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1 (delta double-applied)", snap[0].ToolCallCount)
	}
}

func TestPollOnce_ConcurrentCallsApplyDeltaOnce(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := newCursorSource(clk.Now())
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := m.PollOnce(context.Background()); err != nil {
				t.Errorf("PollOnce: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assertSingleApply(t, m)
}

func TestPollOnce_RunLoopAndExternalPollDoNotDoubleApply(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := newCursorSource(clk.Now())
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	waitForRunTicker(t, clk)

	clk.Advance(time.Second) // Run's tick starts a poll
	deadline := time.Now().Add(2 * time.Second)
	for src.parses.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never polled")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.PollOnce(context.Background()); err != nil { // external poll racing Run's
		t.Fatalf("PollOnce: %v", err)
	}
	cancel()
	_ = waitForDone(t, done)

	assertSingleApply(t, m)
}

func TestPollOnce_BlockedOnPollLockHonorsContext(t *testing.T) {
	t.Parallel()
	clk := testClock()
	src := newCursorSource(clk.Now())
	src.entered = make(chan struct{}, 1)
	src.release = make(chan struct{})
	m, err := monitor.New(monitor.WithSources(src), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := make(chan error, 1)
	go func() { first <- m.PollOnce(context.Background()) }()
	<-src.entered // first poll now holds the poll lock inside Parse

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- m.PollOnce(ctx) }()
	cancel()

	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("blocked PollOnce error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PollOnce with canceled ctx blocked on the poll lock")
	}

	close(src.release)
	if err := <-first; err != nil {
		t.Errorf("first PollOnce: %v", err)
	}
	if got := src.parses.Load(); got != 1 {
		t.Errorf("Parse calls = %d, want 1 (canceled poll must not parse)", got)
	}
}
