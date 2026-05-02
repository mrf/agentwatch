// Package testutil provides lightweight helpers for writing tests in agentwatch.
// It is internal — do not promote to a public package without a concrete consumer need.
package testutil

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// RequireNoError fails t immediately if err is non-nil.
func RequireNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// RequireEqual fails t immediately if want != got.
func RequireEqual[T comparable](t testing.TB, want, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// TempDir returns a temporary directory that is automatically removed when t
// ends. It is a thin wrapper around t.TempDir.
func TempDir(t testing.TB) string {
	t.Helper()
	return t.TempDir()
}

// WriteFixture writes data to filepath.Join(dir, name) with permissions 0600
// and returns the full path. The test fails if the write fails.
func WriteFixture(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("WriteFixture: %v", err)
	}
	return path
}

// LoadFixture reads testdata/<name> relative to the calling test's working
// directory (the package directory). The test fails if the file cannot be read.
func LoadFixture(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("LoadFixture %q: %v", name, err)
	}
	return data
}

// NewLogger returns an *slog.Logger and a *bytes.Buffer that captures its
// output at Debug level and above. Use buf.String() in assertions to verify
// that specific messages were logged.
func NewLogger(t testing.TB) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// DiscardLogger returns an *slog.Logger that silently discards all output.
// Use it when a logger is required but log output is irrelevant to the test.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
