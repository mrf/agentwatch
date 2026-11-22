package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestDiscover_UsesWalker pins the option doc's promise that Discover runs on
// the shared directory-mtime-caching walker rather than a private WalkDir.
func TestDiscover_UsesWalker(t *testing.T) {
	src, err := New(WithRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cs, ok := src.(*ClaudeSource)
	if !ok {
		t.Fatalf("New returned %T, want *ClaudeSource", src)
	}
	if cs.walker == nil {
		t.Fatal("ClaudeSource.walker is nil; Discover must use filewatch")
	}
}

// TestDiscover_AppendedFileStaysInWindow verifies that a session appended to
// after it aged out of the window is discovered again even though its
// directory mtime never changed (appends only touch the file).
func TestDiscover_AppendedFileStaysInWindow(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-tmp-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(proj, "sess.jsonl")
	if err := os.WriteFile(f, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(f, stale, stale); err != nil {
		t.Fatal(err)
	}

	src, err := New(WithRoot(root), WithDiscoverWindow(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 0 {
		t.Fatalf("stale file discovered: %v", handles)
	}

	now := time.Now()
	if err := os.Chtimes(f, now, now); err != nil {
		t.Fatal(err)
	}
	handles, err = src.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].Path != f {
		t.Fatalf("got %v, want the appended session", handles)
	}
	if handles[0].StartedAt.Before(now.Add(-time.Second)) {
		t.Errorf("StartedAt = %v, want fresh mtime near %v", handles[0].StartedAt, now)
	}
}

// TestDiscover_UnreadableProjectDirKeepsSiblings verifies that one project
// directory the process cannot read does not fail discovery for the rest.
func TestDiscover_UnreadableProjectDirKeepsSiblings(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	root := t.TempDir()
	for _, p := range []string{"-tmp-a", "-tmp-locked", "-tmp-z"} {
		dir := filepath.Join(root, p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sess.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(root, "-tmp-locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	src, err := New(WithRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("got %d handles, want 2 (siblings of the unreadable dir)", len(handles))
	}
}
