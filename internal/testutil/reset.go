package testutil

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mrf/agentwatch/source"
)

// counts are the accumulating fields of an Update.
type counts struct{ msgs, tools, compactions int }

func countsOf(u source.Update) counts {
	return counts{u.MessageCountDelta, u.ToolCallCountDelta, u.CompactionCountDelta}
}

// CheckRewriteResets drives src through the ways a file-tail source can lose
// its place in the transcript at h.Path: truncation below the cursor, an
// in-place rewrite to at least the cursor, and replacement by rename. Each
// must come back as a Reset update whose counts equal a fresh parse of the
// file now on disk. long must be longer than short, the two must differ in
// their first line, and short must yield at least one message.
func CheckRewriteResets(t *testing.T, src source.Source, h source.SessionHandle, long, short string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Dir(h.Path)

	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parse := func(step string, h source.SessionHandle, c source.Cursor) (source.Update, source.Cursor) {
		t.Helper()
		u, next, err := src.Parse(ctx, h, c)
		if err != nil {
			t.Fatalf("%s: Parse: %v", step, err)
		}
		return u, next
	}
	fresh := func(content string) counts {
		t.Helper()
		fh := h
		fh.Path = filepath.Join(dir, "fresh.jsonl")
		write(fh.Path, content)
		u, _ := parse("fresh", fh, "")
		return countsOf(u)
	}
	wantReset := func(step string, u source.Update, want counts) {
		t.Helper()
		if !u.Reset {
			t.Errorf("%s: Reset = false, want true", step)
		}
		if u.SessionID == "" {
			t.Errorf("%s: SessionID empty; the monitor would drop the reset", step)
		}
		if got := countsOf(u); got != want {
			t.Errorf("%s: counts = %+v, want %+v (a fresh parse)", step, got, want)
		}
	}

	rotated := short + long
	wantShort, wantLong, wantRotated := fresh(short), fresh(long), fresh(rotated)
	if wantShort.msgs == 0 {
		t.Fatal("short fixture yields no messages; the test would prove nothing")
	}

	write(h.Path, long)
	u, c := parse("initial", h, "")
	if u.Reset {
		t.Error("initial: Reset = true on a fresh cursor")
	}

	write(h.Path, short)
	u, c = parse("truncate", h, c)
	wantReset("truncate", u, wantShort)

	u, c = parse("idle", h, c)
	if u.Reset {
		t.Error("idle: Reset = true with the file unchanged")
	}

	write(h.Path, long)
	u, c = parse("rewrite", h, c)
	wantReset("rewrite", u, wantLong)

	tmp := filepath.Join(dir, "rotated.tmp")
	write(tmp, rotated)
	if err := os.Rename(tmp, h.Path); err != nil {
		t.Fatal(err)
	}
	u, _ = parse("rotate", h, c)
	wantReset("rotate", u, wantRotated)
}

// CorruptingSource wraps a Source and, while corruption is on, hands Parse an
// unparseable cursor in place of the one the monitor persisted. It models a
// cursor damaged between polls.
type CorruptingSource struct {
	source.Source
	corrupt atomic.Bool
}

// SetCorrupt turns cursor corruption on or off.
func (s *CorruptingSource) SetCorrupt(on bool) { s.corrupt.Store(on) }

// Parse implements source.Source.
func (s *CorruptingSource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.Update, source.Cursor, error) {
	if s.corrupt.Load() {
		c = "{corrupt"
	}
	return s.Source.Parse(ctx, h, c)
}
