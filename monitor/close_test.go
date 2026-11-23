package monitor_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// closerSource is a mock source that also implements io.Closer.
type closerSource struct {
	*mock.Source
	closes   atomic.Int32
	closeErr error
	name     string
	order    *closeOrder
}

type closeOrder struct {
	mu    sync.Mutex
	names []string
}

func (o *closeOrder) add(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.names = append(o.names, name)
}

func (s *closerSource) Close() error {
	s.closes.Add(1)
	if s.order != nil {
		s.order.add(s.name)
	}
	return s.closeErr
}

func runUntilCanceled(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	cancel()
	if err := waitForDone(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestRun_ClosesCloserSourcesOnExit(t *testing.T) {
	t.Parallel()
	order := &closeOrder{}
	a := &closerSource{Source: mock.New(mock.WithName("a")), name: "a", order: order}
	b := &closerSource{Source: mock.New(mock.WithName("b")), name: "b", order: order}
	plain := mock.New(mock.WithName("plain"))

	m, err := monitor.New(
		monitor.WithSources(a, plain, b),
		monitor.WithClock(testClock()),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runUntilCanceled(t, m)

	if got := a.closes.Load(); got != 1 {
		t.Errorf("a closed %d times, want 1", got)
	}
	if got := b.closes.Load(); got != 1 {
		t.Errorf("b closed %d times, want 1", got)
	}
	if len(order.names) != 2 || order.names[0] != "a" || order.names[1] != "b" {
		t.Errorf("close order = %v, want [a b]", order.names)
	}
}

func TestRun_ClosesWhenAlreadyCanceled(t *testing.T) {
	t.Parallel()
	src := &closerSource{Source: mock.New(mock.WithName("a"))}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(testClock()),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = m.Run(ctx)
	if got := src.closes.Load(); got != 1 {
		t.Errorf("closed %d times, want 1", got)
	}
}

func TestRun_CloseErrorIsLoggedNotReturned(t *testing.T) {
	t.Parallel()
	const secret = "/home/user/secret/opencode.db"
	failing := &closerSource{Source: mock.New(mock.WithName("a")), closeErr: errors.New("close " + secret)}
	after := &closerSource{Source: mock.New(mock.WithName("b"))}

	var buf bytes.Buffer
	m, err := monitor.New(
		monitor.WithSources(failing, after),
		monitor.WithClock(testClock()),
		monitor.WithPollInterval(time.Second),
		monitor.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// runUntilCanceled asserts Run still returns context.Canceled.
	runUntilCanceled(t, m)

	if got := after.closes.Load(); got != 1 {
		t.Errorf("source after failing closer closed %d times, want 1", got)
	}
	if !strings.Contains(buf.String(), "source close failed") {
		t.Errorf("close error not logged: %q", buf.String())
	}
	for name, h := range m.Health() {
		if strings.Contains(h.LastError, secret) {
			t.Errorf("health %s leaked close error: %q", name, h.LastError)
		}
	}
}

func TestPollOnce_DoesNotCloseSources(t *testing.T) {
	t.Parallel()
	src := &closerSource{Source: mock.New(mock.WithName("a"))}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(testClock()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := src.closes.Load(); got != 0 {
		t.Errorf("PollOnce closed source %d times, want 0", got)
	}
}

// lazySource models a source (like opencode) whose Close is not terminal: the
// next Discover reopens the handle, so a Close that lands before an in-flight
// Discover finishes leaves a handle open after Close returned.
type lazySource struct {
	name    string
	entered chan struct{} // closed once Discover is running
	release chan struct{} // Discover blocks until this is closed

	mu   sync.Mutex
	open bool
}

func newLazySource() *lazySource {
	return &lazySource{name: "lazy", entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *lazySource) Name() string { return s.name }

func (s *lazySource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	s.open = true
	s.mu.Unlock()
	return nil, nil
}

func (s *lazySource) Parse(_ context.Context, _ source.SessionHandle, c source.Cursor) (source.Update, source.Cursor, error) {
	return source.Update{}, c, nil
}

func (s *lazySource) Close() error {
	s.mu.Lock()
	s.open = false
	s.mu.Unlock()
	return nil
}

func (s *lazySource) isOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

func newLazyMonitor(t *testing.T, src *lazySource) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(testClock()),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestRun_CloseWaitsForInFlightPollOnce(t *testing.T) {
	t.Parallel()
	src := newLazySource()
	m := newLazyMonitor(t, src)

	pollDone := make(chan error, 1)
	go func() { pollDone <- m.PollOnce(context.Background()) }()
	<-src.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	select {
	case err := <-runDone:
		t.Fatalf("Run returned %v while an external PollOnce was still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(src.release)
	if err := waitForDone(t, pollDone); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if err := waitForDone(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if src.isOpen() {
		t.Error("source left open after Run returned: in-flight PollOnce reopened it after Close")
	}
}

func TestRun_CloseBoundedByInFlightPollContext(t *testing.T) {
	t.Parallel()
	src := newLazySource()
	m := newLazyMonitor(t, src)

	pollCtx, cancelPoll := context.WithCancel(context.Background())
	pollDone := make(chan error, 1)
	go func() { pollDone <- m.PollOnce(pollCtx) }()
	<-src.entered

	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(runCtx) }()

	// Run is waiting on the poll; canceling the poll's ctx must unblock both.
	cancelPoll()
	// PollOnce's own result is not under test: it may swallow the source's
	// ctx error. What matters is that it returns and Run follows.
	_ = waitForDone(t, pollDone)
	if err := waitForDone(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if src.isOpen() {
		t.Error("source left open after Run returned")
	}
}
