//go:build unix

package jsonl

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// readBounded runs ReadLines under a deadline. If it blocks (the regression),
// it unblocks the stuck open(2) so the goroutine does not outlive the test.
func readBounded(t *testing.T, readPath, fifo string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadLines(readPath, 0, defaultOpts)
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("Read blocked on a FIFO")
		return nil
	}
}

func TestRead_FIFORejectedWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	if err := readBounded(t, fifo, fifo); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("err = %v, want ErrNotRegular", err)
	}
}

func TestRead_SymlinkToFIFORejected(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if err := readBounded(t, link, fifo); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("err = %v, want ErrNotRegular", err)
	}
}
