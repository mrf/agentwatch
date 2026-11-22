package opencode_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/opencode"
)

type lifecycleRecorder struct {
	events []session.LifecycleEvent
}

func (r *lifecycleRecorder) HandleEvent(_ context.Context, ev monitor.Event) error {
	if ev.Type == monitor.EventLifecycle && ev.Lifecycle != nil {
		r.events = append(r.events, *ev.Lifecycle)
	}
	return nil
}

func (r *lifecycleRecorder) count(typ session.LifecycleEventType) int {
	n := 0
	for i := 0; i < len(r.events); i++ {
		if r.events[i].Type == typ {
			n++
		}
	}
	return n
}

func newMonitor(t *testing.T, src source.Source, clk *clock.Mock, rec *lifecycleRecorder, opts ...monitor.Option) *monitor.Monitor {
	t.Helper()
	all := append([]monitor.Option{
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithSink(rec),
		monitor.WithCompletionRetention(24 * time.Hour),
	}, opts...)
	m, err := monitor.New(all...)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	return m
}

func newSource(t *testing.T, dbPath string) *opencode.Source {
	t.Helper()
	src := opencode.New(opencode.WithDBPath(dbPath))
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestIdleSessionGoesStaleUnderMonitor(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-idle", "idle", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-idle", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})
	src := newSource(t, dbPath)

	clk := clock.NewMock(time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC))
	rec := &lifecycleRecorder{}
	m := newMonitor(t, src, clk, rec, monitor.WithStaleThreshold(10*time.Minute))
	ctx := context.Background()

	for i := 0; i < 15; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
		clk.Advance(time.Minute)
	}

	if got := rec.count(session.EventStale); got != 1 {
		t.Errorf("stale events = %d, want 1 (events: %+v)", got, rec.events)
	}
	st, ok := m.Get("sess-idle")
	if !ok {
		t.Fatal("session missing from store")
	}
	if st.Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want terminal", st.Lifecycle)
	}
}

func TestArchivedSessionYieldsOneTerminalEvent(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-done", "done", "/project",
			`{"id":"model"}`, 8000, 500,
			"2026-01-15T10:00:00Z", "2026-01-15T11:00:00Z", "2026-01-15T11:00:00Z")
	})
	src := newSource(t, dbPath)

	clk := clock.NewMock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	rec := &lifecycleRecorder{}
	m := newMonitor(t, src, clk, rec)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
		clk.Advance(time.Second)
	}

	if got := rec.count(session.EventTerminal); got != 1 {
		t.Errorf("terminal events = %d, want 1 (events: %+v)", got, rec.events)
	}
	if got := rec.count(session.EventResumed); got != 0 {
		t.Errorf("resumed events = %d, want 0 (events: %+v)", got, rec.events)
	}
	st, _ := m.Get("sess-done")
	if st.Lifecycle != session.LifecycleTerminal {
		t.Errorf("Lifecycle = %q, want terminal", st.Lifecycle)
	}
}

func TestSessionArchivedWhileTrackedYieldsOneTerminalEvent(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "s", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})
	src := newSource(t, dbPath)

	clk := clock.NewMock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	rec := &lifecycleRecorder{}
	m := newMonitor(t, src, clk, rec)
	ctx := context.Background()

	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	addDataToDB(t, dbPath, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE session SET time_archived = '2026-01-15T11:00:00Z' WHERE id = 'sess-1'`); err != nil {
			t.Fatal(err)
		}
	})
	for i := 0; i < 8; i++ {
		clk.Advance(time.Second)
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}

	if got := rec.count(session.EventTerminal); got != 1 {
		t.Errorf("terminal events = %d, want 1 (events: %+v)", got, rec.events)
	}
	if got := rec.count(session.EventResumed); got != 0 {
		t.Errorf("resumed events = %d, want 0 (events: %+v)", got, rec.events)
	}
}

func TestParseNoChangeReturnsZeroUpdate(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "msg-1", "sess-1", `{"role":"user"}`,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
	})
	src := newSource(t, dbPath)
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	u1, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if u1.SessionID == "" {
		t.Fatal("first parse must report the session")
	}
	u2, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatal(err)
	}
	if u2.SessionID != "" {
		t.Errorf("unchanged session returned non-zero update: %+v", u2)
	}
	if c2 != c1 {
		t.Errorf("cursor changed without data: %q -> %q", c1, c2)
	}
}

func TestParseTokenChangeIsReported(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	})
	src := newSource(t, dbPath)
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	addDataToDB(t, dbPath, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE session SET tokens_input = 900 WHERE id = 'sess-1'`); err != nil {
			t.Fatal(err)
		}
	})
	u2, _, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatal(err)
	}
	if u2.SessionID == "" {
		t.Fatal("token change should produce an update")
	}
	assertInt(t, "ContextTokens", u2.ContextTokens, 900)
}

func TestParseLegacyCursorAccepted(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	})
	src := newSource(t, dbPath)
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}

	if _, _, err := src.Parse(context.Background(), h, "0,0"); err != nil {
		t.Fatalf("legacy cursor rejected: %v", err)
	}
}

func TestDBPathWithSpecialCharsOpens(t *testing.T) {
	for _, name := range []string{"what?ever", "a#b", "100%", "sp ace"} {
		t.Run(name, func(t *testing.T) {
			// Seed at a plain path (the driver would parse '?' in a raw DSN),
			// then move the file under the awkward directory name.
			plain := seedFileDB(t, func(db *sql.DB) {
				insertSession(t, db, "sess-1", "s", "/p", `{"id":"m"}`, 1, 1,
					"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
			})
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "opencode.db")
			if err := os.Rename(plain, dbPath); err != nil {
				t.Fatal(err)
			}

			src := newSource(t, dbPath)
			handles, err := src.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(handles) != 1 {
				t.Fatalf("handles = %d, want 1", len(handles))
			}
			u, _, err := src.Parse(context.Background(), handles[0], "")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			assertEqual(t, "SessionID", u.SessionID, "sess-1")
		})
	}
}

func TestRelativeDBPathOpens(t *testing.T) {
	plain := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "s", "/p", `{"id":"m"}`, 1, 1,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	})
	t.Chdir(filepath.Dir(plain))
	src := newSource(t, filepath.Base(plain))
	handles, err := src.Discover(context.Background())
	if err != nil || len(handles) != 1 {
		t.Fatalf("Discover = %v, %v", handles, err)
	}
}

func TestDBFileCreatedAfterFirstPollIsPickedUp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	src := newSource(t, dbPath)

	if h, err := src.Discover(context.Background()); err != nil || len(h) != 0 {
		t.Fatalf("Discover on missing db = %v, %v", h, err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	insertSession(t, db, "sess-1", "s", "/p", `{"id":"m"}`, 1, 1,
		"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	_ = db.Close()

	h, err := src.Discover(context.Background())
	if err != nil || len(h) != 1 {
		t.Fatalf("Discover after create = %v, %v", h, err)
	}
}

func TestReplacedDBFileIsReopened(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	build := func(id string) {
		if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(testSchema); err != nil {
			t.Fatal(err)
		}
		insertSession(t, db, id, "s", "/p", `{"id":"m"}`, 1, 1,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		_ = db.Close()
	}
	build("old")
	src := newSource(t, dbPath)
	if h, err := src.Discover(context.Background()); err != nil || len(h) != 1 || h[0].ID != "old" {
		t.Fatalf("first Discover = %v, %v", h, err)
	}
	build("new")
	h, err := src.Discover(context.Background())
	if err != nil || len(h) != 1 || h[0].ID != "new" {
		t.Fatalf("Discover after replace = %v, %v", h, err)
	}
}

func TestCancelledContextAborts(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "sess-1", "", "/project",
			`{"id":"model"}`, 100, 10,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
	})
	src := newSource(t, dbPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Discover(ctx); err == nil {
		t.Error("Discover with cancelled ctx: expected error")
	}
	h := source.SessionHandle{ID: "sess-1", Path: dbPath, Source: "opencode"}
	if _, _, err := src.Parse(ctx, h, ""); err == nil {
		t.Error("Parse with cancelled ctx: expected error")
	}
}
