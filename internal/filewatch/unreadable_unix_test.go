//go:build unix

package filewatch_test

import (
	"os"
	"path/filepath"
	"testing"
)

// denyDir makes dir unreadable and restores its mode on cleanup so temp-dir
// removal succeeds. It skips when permissions are not enforced (root).
func denyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("directory still readable after chmod 000")
	}
}

// TestUnreadableSubdirDoesNotAbortDiscover verifies that one unreadable
// subdirectory does not hide sessions in its siblings, on first and repeat
// scans.
func TestUnreadableSubdirDoesNotAbortDiscover(t *testing.T) {
	root := makeTree(t, map[string]string{
		"a/ok.jsonl":     "{}\n",
		"locked/x.jsonl": "{}\n",
		"z/ok.jsonl":     "{}\n",
	})
	denyDir(t, filepath.Join(root, "locked"))

	w := newWalker(root, false)
	want := []string{filepath.Join(root, "a", "ok.jsonl"), filepath.Join(root, "z", "ok.jsonl")}
	for i := 0; i < 2; i++ {
		assertPaths(t, discoverBounded(t, w), want)
	}
}

// TestUnreadableSubdirRecovers verifies a skipped directory is retried, not
// cached as empty, once it becomes readable again.
func TestUnreadableSubdirRecovers(t *testing.T) {
	root := makeTree(t, map[string]string{
		"a/ok.jsonl":     "{}\n",
		"locked/x.jsonl": "{}\n",
	})
	locked := filepath.Join(root, "locked")
	denyDir(t, locked)

	w := newWalker(root, false)
	assertPaths(t, discoverBounded(t, w), []string{filepath.Join(root, "a", "ok.jsonl")})

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	assertPaths(t, discoverBounded(t, w), []string{
		filepath.Join(root, "a", "ok.jsonl"),
		filepath.Join(locked, "x.jsonl"),
	})
}

// TestUnreadableRootIsAnError verifies an unreadable root is still fatal: it
// is a configuration problem, not a removal race.
func TestUnreadableRootIsAnError(t *testing.T) {
	root := makeTree(t, map[string]string{"a/ok.jsonl": "{}\n"})
	denyDir(t, root)

	if _, err := newWalker(root, false).Discover(t.Context()); err == nil {
		t.Fatal("Discover on unreadable root returned nil error")
	}
}
