package monitor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/antigravity"
	"github.com/mrf/agentwatch/sources/mock"
)

// The monitor must persist whatever cursor Parse returns — on the data path,
// the empty-update path, and the error path — so sources that advance past
// unreportable input are not re-read every poll.

func newCursorTestMonitor(t *testing.T, src source.Source) *monitor.Monitor {
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

func TestPollOnce_EmptyUpdatePersistsAdvancedCursor(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	// Zero update (SessionID == "") but the cursor advanced to B.
	src.AddUpdate("s1", source.SourceUpdate{}, "B")

	m := newCursorTestMonitor(t, src)
	for i := 0; i < 2; i++ {
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}

	calls := src.ParseCalls()
	if len(calls) != 2 {
		t.Fatalf("Parse calls: got %d, want 2", len(calls))
	}
	if calls[0].Cursor != "" {
		t.Errorf("first Parse cursor: got %q, want empty", calls[0].Cursor)
	}
	if calls[1].Cursor != "B" {
		t.Errorf("second Parse cursor: got %q, want %q (advance on empty update was dropped)", calls[1].Cursor, "B")
	}
	if got := len(m.Snapshot()); got != 0 {
		t.Errorf("empty update must not create a session, got %d", got)
	}
}

func TestPollOnce_ParseErrorPersistsReturnedCursor(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	src.QueueParseResult("s1", source.SourceUpdate{}, "past-bad-input", errors.New("bad file"))

	m := newCursorTestMonitor(t, src)
	for i := 0; i < 2; i++ {
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}

	calls := src.ParseCalls()
	if len(calls) != 2 {
		t.Fatalf("Parse calls: got %d, want 2", len(calls))
	}
	if calls[1].Cursor != "past-bad-input" {
		t.Errorf("second Parse cursor: got %q, want %q (cursor on error path was dropped)", calls[1].Cursor, "past-bad-input")
	}
}

func TestPollOnce_ParseErrorReturningInputCursorRetries(t *testing.T) {
	t.Parallel()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test"}})
	// A source that wants a retry returns the cursor it was given.
	src.QueueParseResult("s1", source.SourceUpdate{}, "A", nil)
	src.QueueParseResult("s1", source.SourceUpdate{}, "A", errors.New("transient"))

	m := newCursorTestMonitor(t, src)
	for i := 0; i < 3; i++ {
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}

	calls := src.ParseCalls()
	if len(calls) != 3 {
		t.Fatalf("Parse calls: got %d, want 3", len(calls))
	}
	if calls[2].Cursor != "A" {
		t.Errorf("retry Parse cursor: got %q, want %q", calls[2].Cursor, "A")
	}
}

// countingSource wraps a source and counts Parse calls that returned an error.
type countingSource struct {
	source.Source
	parseErrors atomic.Int32
}

func (c *countingSource) Parse(ctx context.Context, h source.SessionHandle, cur source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	u, next, err := c.Source.Parse(ctx, h, cur)
	if err != nil {
		c.parseErrors.Add(1)
	}
	return u, next, err
}

func TestPollOnce_AntigravityBadFileParsedOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bad := filepath.Join(root, "11111111-2222-3333-4444-555555555555.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad file: %v", err)
	}

	inner, err := antigravity.New(antigravity.WithRoot(root))
	if err != nil {
		t.Fatalf("antigravity.New: %v", err)
	}
	src := &countingSource{Source: inner}
	m := newCursorTestMonitor(t, src)

	for i := 0; i < 4; i++ {
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}

	if got := src.parseErrors.Load(); got != 1 {
		t.Errorf("bad file parse failures: got %d, want 1 (re-read every poll)", got)
	}
	// Once the bad file stops failing, the source's health recovers.
	h := m.Health()["antigravity"]
	if h.ParseFailures != 0 || h.Status != monitor.HealthHealthy {
		t.Errorf("health did not recover: %+v", h)
	}
}
