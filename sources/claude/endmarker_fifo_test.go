//go:build !windows

package claude

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDrainEndMarkers_FIFODoesNotBlock(t *testing.T) {
	markerDir := t.TempDir()

	// A FIFO whose name matches the marker pattern: opening/reading it would
	// block until a writer appears, while holding s.mu.
	fifo := filepath.Join(markerDir, "pipe.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	writeEndMarker(t, markerDir, "real.json", map[string]string{
		"session_id": "real",
		"reason":     "logout",
	})

	src := newDrainSource(t, markerDir)

	done := make(chan struct{})
	go func() {
		defer close(done)
		src.drainEndMarkers(time.Now())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainEndMarkers blocked on a FIFO")
	}

	// The lock must be free, the real marker consumed, and the FIFO left alone.
	if _, ok := src.consumePendingTerminal("real"); !ok {
		t.Error("regular marker next to the FIFO was not drained")
	}
	fi, err := os.Lstat(fifo)
	if err != nil {
		t.Fatalf("FIFO was removed: %v", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("FIFO replaced by %v", fi.Mode())
	}
}
