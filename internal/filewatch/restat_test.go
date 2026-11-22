package filewatch_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
)

// TestCacheHitRestatsFileMtime verifies that a file appended to after the
// first scan is judged by its current mtime, not the cached one. Appending
// changes the file's mtime but not its directory's, so the directory cache
// hits on the second Discover.
func TestCacheHitRestatsFileMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	f := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(f, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-2 * time.Hour)
	if err := os.Chtimes(f, stale, stale); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		filewatch.WithMaxAge(time.Hour),
		filewatch.WithClock(func() time.Time { return now }),
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("first Discover: got %v, want none (file is outside the window)", paths(entries))
	}

	fresh := now.Add(-time.Minute)
	if err := os.Chtimes(f, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("test premise broken: dir mtime changed (%v -> %v)", before.ModTime(), after.ModTime())
	}

	entries, err = w.Discover(context.Background())
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != f {
		t.Fatalf("second Discover: got %v, want [%s]", paths(entries), f)
	}
	if !entries[0].ModTime.Equal(fresh) {
		t.Errorf("Entry.ModTime = %v, want fresh mtime %v", entries[0].ModTime, fresh)
	}
}

// TestCacheHitDropsRemovedFile verifies that a cached file that vanished
// between scans is not reported, even when the directory mtime is restored so
// that only the per-file re-stat can notice.
func TestCacheHitDropsRemovedFile(t *testing.T) {
	root := makeTree(t, map[string]string{"a.jsonl": "x", "b.jsonl": "x"})
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))
	if _, err := w.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := paths(entries)
	if len(got) != 1 || got[0] != filepath.Join(root, "b.jsonl") {
		t.Errorf("got %v, want only b.jsonl", got)
	}
}

// TestSkipDir verifies that directories rejected by WithSkipDir are not
// descended into, on both a fresh scan and a cached rescan.
func TestSkipDir(t *testing.T) {
	root := makeTree(t, map[string]string{
		"proj/a.jsonl":             "x",
		"proj/a/subagents/b.jsonl": "x",
		"proj/a/c.jsonl":           "x",
	})
	skip := func(path string) bool { return filepath.Base(path) == "subagents" }
	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"), filewatch.WithSkipDir(skip))

	want := []string{
		filepath.Join(root, "proj", "a", "c.jsonl"),
		filepath.Join(root, "proj", "a.jsonl"),
	}
	sort.Strings(want)
	for pass := 1; pass <= 2; pass++ {
		entries, err := w.Discover(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		got := paths(entries)
		sort.Strings(got)
		if len(got) != len(want) {
			t.Fatalf("pass %d: got %v, want %v", pass, got, want)
		}
		for i := 0; i < len(want); i++ {
			if got[i] != want[i] {
				t.Fatalf("pass %d: got %v, want %v", pass, got, want)
			}
		}
	}
}
