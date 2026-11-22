//go:build unix

package filewatch_test

import (
	"path/filepath"
	"syscall"
	"testing"
)

// TestDiscoverSkipsNonRegularFiles verifies that a FIFO, and a symlink to a
// FIFO, are never reported even when their names match the pattern, because
// opening one would block the poll.
func TestDiscoverSkipsNonRegularFiles(t *testing.T) {
	root := makeTree(t, map[string]string{"real.jsonl": "{}\n"})
	fifo := filepath.Join(root, "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	mustSymlink(t, fifo, filepath.Join(root, "pipe-link.jsonl"))

	got := discoverBounded(t, newWalker(root, true))

	assertPaths(t, got, []string{filepath.Join(root, "real.jsonl")})
}
