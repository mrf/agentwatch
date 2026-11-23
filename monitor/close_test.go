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
