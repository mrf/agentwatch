package jsonl_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrf/agentwatch/jsonl"
)

func visitAll(*jsonl.Entry, []byte) bool { return true }

func TestForEachEntry_OversizedFileIsErrFileTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate makes a sparse file, so this costs no real disk.
	if err := f.Truncate(jsonl.MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := jsonl.ForEachEntry(path, 0, visitAll); !errors.Is(err, jsonl.ErrFileTooLarge) {
		t.Fatalf("ForEachEntry error = %v, want ErrFileTooLarge", err)
	}
}

func TestForEachEntry_MissingFileMatchesErrNotExist(t *testing.T) {
	_, err := jsonl.ForEachEntry(filepath.Join(t.TempDir(), "absent.jsonl"), 0, visitAll)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ForEachEntry error = %v, want fs.ErrNotExist in chain", err)
	}
}
