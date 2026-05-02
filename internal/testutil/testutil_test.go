package testutil_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/testutil"
)

func TestRequireNoError_Nil(t *testing.T) {
	// A nil error must not cause the test to fail.
	testutil.RequireNoError(t, nil)
}

func TestRequireEqual_Equal(t *testing.T) {
	testutil.RequireEqual(t, 42, 42)
	testutil.RequireEqual(t, "hello", "hello")
	testutil.RequireEqual(t, true, true)
}

func TestTempDir_IsDirectory(t *testing.T) {
	dir := testutil.TempDir(t)
	fi, err := os.Stat(dir)
	testutil.RequireNoError(t, err)
	if !fi.IsDir() {
		t.Errorf("TempDir() = %q, want a directory", dir)
	}
}

func TestWriteFixture_CreatesFile(t *testing.T) {
	dir := testutil.TempDir(t)
	want := []byte("hello fixture")
	path := testutil.WriteFixture(t, dir, "example.txt", want)

	wantPath := filepath.Join(dir, "example.txt")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}

	got, err := os.ReadFile(path)
	testutil.RequireNoError(t, err)
	if !bytes.Equal(want, got) {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestLoadFixture_ReadsFile(t *testing.T) {
	// testdata/sample.txt is committed alongside this test file.
	got := testutil.LoadFixture(t, "sample.txt")
	want := []byte("fixture content")
	if !bytes.Equal(want, got) {
		t.Errorf("LoadFixture = %q, want %q", got, want)
	}
}

func TestNewLogger_CapturesOutput(t *testing.T) {
	logger, buf := testutil.NewLogger(t)
	logger.Info("test message", "key", "value")
	out := buf.String()
	if !strings.Contains(out, "test message") {
		t.Errorf("log output %q does not contain 'test message'", out)
	}
	if !strings.Contains(out, "key=value") {
		t.Errorf("log output %q does not contain 'key=value'", out)
	}
}

func TestNewLogger_CapturesDebug(t *testing.T) {
	logger, buf := testutil.NewLogger(t)
	logger.Debug("debug msg")
	if !strings.Contains(buf.String(), "debug msg") {
		t.Errorf("debug log not captured: %q", buf.String())
	}
}

func TestDiscardLogger_DoesNotPanic(t *testing.T) {
	logger := testutil.DiscardLogger()
	logger.Info("should be silently discarded")
	logger.Debug("also discarded")
	logger.Error("still discarded")
}
