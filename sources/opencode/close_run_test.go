package opencode_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/opencode"
)

// openFDsOf counts this process's open file descriptors that resolve to path.
// It skips the test where /proc/self/fd is unavailable.
func openFDsOf(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("/proc/self/fd unavailable")
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && target == path {
			n++
		}
	}
	return n
}

func TestRun_ReleasesDBHandleOnExit(t *testing.T) {
	dbPath := seedFileDB(t, func(*sql.DB) {})
	resolved, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately no t.Cleanup(src.Close): the monitor must release it.
	src := opencode.New(opencode.WithDBPath(dbPath))
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clock.NewMock(time.Now())),
		monitor.WithPollInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if openFDsOf(t, resolved) == 0 {
		t.Fatal("precondition: source should hold the DB open after a poll")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}

	if n := openFDsOf(t, resolved); n != 0 {
		t.Errorf("%d fds still open on the DB after Run returned", n)
	}
}
