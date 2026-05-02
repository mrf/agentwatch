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

// makeTree creates a temporary directory tree for testing.
// The returned cleanup func removes it.
func makeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

func paths(entries []filewatch.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	sort.Strings(out)
	return out
}

// TestDiscoverByPattern verifies that only files matching the glob pattern
// are returned.
func TestDiscoverByPattern(t *testing.T) {
	root := makeTree(t, map[string]string{
		"a/session.jsonl": "data",
		"a/notes.txt":     "notes",
		"b/other.jsonl":   "data",
		"b/readme.md":     "docs",
	})

	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := paths(entries)
	want := []string{
		filepath.Join(root, "a", "session.jsonl"),
		filepath.Join(root, "b", "other.jsonl"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("entry[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiscoverNoPattern returns all files when no pattern is set.
func TestDiscoverNoPattern(t *testing.T) {
	root := makeTree(t, map[string]string{
		"a/session.jsonl": "data",
		"b/other.txt":     "text",
	})

	w := filewatch.New([]string{root})
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

// TestAgeFiltering verifies that files older than maxAge are excluded.
func TestAgeFiltering(t *testing.T) {
	root := t.TempDir()

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	// Write two files.
	recent := filepath.Join(root, "recent.jsonl")
	old := filepath.Join(root, "old.jsonl")

	if err := os.WriteFile(recent, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Back-date the old file to 2 hours ago.
	oldTime := now.Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// Set the recent file to 10 minutes ago.
	recentTime := now.Add(-10 * time.Minute)
	if err := os.Chtimes(recent, recentTime, recentTime); err != nil {
		t.Fatal(err)
	}

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		filewatch.WithMaxAge(1*time.Hour),
		filewatch.WithClock(func() time.Time { return now }),
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := paths(entries)
	if len(got) != 1 {
		t.Fatalf("got %v, want only recent file", got)
	}
	if got[0] != recent {
		t.Errorf("got %q, want %q", got[0], recent)
	}
}

// TestAgeFilteringZeroMeansNoLimit verifies that maxAge==0 applies no age
// filter.
func TestAgeFilteringZeroMeansNoLimit(t *testing.T) {
	root := t.TempDir()

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	f := filepath.Join(root, "old.jsonl")
	if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := now.Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(f, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		// WithMaxAge(0) means no filtering — default.
		filewatch.WithClock(func() time.Time { return now }),
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
}

// TestEmptyDirectory verifies that an empty root directory returns no entries
// without error.
func TestEmptyDirectory(t *testing.T) {
	root := t.TempDir()

	w := filewatch.New([]string{root})
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

// TestNonExistentRootIsSkipped verifies that a missing root directory is
// silently skipped rather than returning an error.
func TestNonExistentRootIsSkipped(t *testing.T) {
	w := filewatch.New([]string{"/no/such/directory/ever"})
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover returned error for missing root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

// TestMultipleRoots verifies that files from multiple roots are combined.
func TestMultipleRoots(t *testing.T) {
	root1 := makeTree(t, map[string]string{
		"a.jsonl": "data",
	})
	root2 := makeTree(t, map[string]string{
		"b.jsonl": "data",
	})

	w := filewatch.New([]string{root1, root2}, filewatch.WithPattern("*.jsonl"))
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

// TestSymlinkToFile verifies that a symlink pointing to a regular file is
// followed and the target is returned.
func TestSymlinkToFile(t *testing.T) {
	root := t.TempDir()

	real := filepath.Join(root, "real.jsonl")
	if err := os.WriteFile(real, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Both real.jsonl and link.jsonl match the pattern.
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2: %v", len(entries), paths(entries))
	}
}

// TestSymlinkToDirectory verifies that when symlinks to directories are
// followed, files inside are discovered.
func TestSymlinkToDirectory(t *testing.T) {
	// Build: realdir/session.jsonl and a symlink subdir -> realdir.
	outer := t.TempDir()
	realDir := filepath.Join(outer, "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "session.jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	linkDir := filepath.Join(root, "subdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		filewatch.WithFollowSymlinks(true),
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1: %v", len(entries), paths(entries))
	}
}

// TestSymlinkToDirNotFollowedByDefault verifies that symlinked directories
// are NOT followed when WithFollowSymlinks is not set.
func TestSymlinkToDirNotFollowedByDefault(t *testing.T) {
	outer := t.TempDir()
	realDir := filepath.Join(outer, "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "session.jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	linkDir := filepath.Join(root, "subdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skip("symlinks not supported:", err)
	}

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		// WithFollowSymlinks defaults to false.
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0 (symlinked dir should not be followed): %v", len(entries), paths(entries))
	}
}

// TestEfficientRescan verifies that Discover can be called multiple times and
// that the results are correct across calls even when directories change.
// A short sleep ensures the filesystem mtime advances between the initial scan
// and the new-file write; directory mtime resolution on some systems is ~4 ms.
func TestEfficientRescan(t *testing.T) {
	root := makeTree(t, map[string]string{
		"a.jsonl": "data",
		"b.jsonl": "data",
	})

	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))

	entries1, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	if len(entries1) != 2 {
		t.Fatalf("first Discover: got %d, want 2", len(entries1))
	}

	// Sleep long enough for the directory mtime to advance before adding a new
	// file.  On this WSL2/ext4 system mtime resolution is ~4 ms; 20 ms is a
	// comfortable margin without meaningfully slowing the suite.
	time.Sleep(20 * time.Millisecond)

	// Add a new file.
	if err := os.WriteFile(filepath.Join(root, "c.jsonl"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries2, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if len(entries2) != 3 {
		t.Errorf("second Discover: got %d, want 3", len(entries2))
	}
}

// TestEfficientRescanSubdir verifies that a file added to a subdirectory is
// discovered on the next Discover call even when the parent directory's mtime
// has not changed (only the subdirectory mtime changes).
func TestEfficientRescanSubdir(t *testing.T) {
	root := makeTree(t, map[string]string{
		"sub/a.jsonl": "data",
	})

	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))

	entries1, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	if len(entries1) != 1 {
		t.Fatalf("first Discover: got %d, want 1", len(entries1))
	}

	// Sleep so the subdir mtime advances before we add a new file to it.
	time.Sleep(20 * time.Millisecond)

	sub := filepath.Join(root, "sub")
	if err := os.WriteFile(filepath.Join(sub, "b.jsonl"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries2, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if len(entries2) != 2 {
		t.Errorf("second Discover: got %d, want 2 (subdir file should be found)", len(entries2))
	}
}

// TestContextCancellation verifies that a cancelled context causes Discover
// to return an error.
func TestContextCancellation(t *testing.T) {
	// Build a large-ish tree to give the walk time to be cancelled.
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		sub := filepath.Join(root, string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 10; j++ {
			f := filepath.Join(sub, filepath.Join("file"+string(rune('a'+j))+".jsonl"))
			if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	w := filewatch.New([]string{root}, filewatch.WithPattern("*.jsonl"))
	_, err := w.Discover(ctx)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

// TestMultiplePatterns verifies that multiple patterns are ORed: a file
// matching any pattern is included.
func TestMultiplePatterns(t *testing.T) {
	root := makeTree(t, map[string]string{
		"session.jsonl": "data",
		"session.json":  "data",
		"notes.txt":     "notes",
	})

	w := filewatch.New(
		[]string{root},
		filewatch.WithPattern("*.jsonl"),
		filewatch.WithPattern("*.json"),
	)
	entries, err := w.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries %v, want 2", len(entries), paths(entries))
	}
}
