package jsonl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates a temp file with the given content and returns its path.
func writeFile(t *testing.T, content string) string {
	return writeFileBytes(t, []byte(content))
}

// writeFileBytes creates a temp file with raw bytes and returns its path.
func writeFileBytes(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

var defaultOpts = Options{}

func TestReadLines_EmptyFile(t *testing.T) {
	path := writeFile(t, "")
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines, got %d", len(lines))
	}
	if nextOffset != 0 {
		t.Errorf("expected nextOffset 0, got %d", nextOffset)
	}
}

func TestReadLines_SingleCompleteLine(t *testing.T) {
	path := writeFile(t, `{"a":1}`+"\n")
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if string(lines[0]) != `{"a":1}` {
		t.Errorf("unexpected line content: %q", lines[0])
	}
	if nextOffset != int64(len(`{"a":1}`)+1) {
		t.Errorf("unexpected nextOffset %d", nextOffset)
	}
}

func TestReadLines_PartialLastLine(t *testing.T) {
	// "line1\npartial" — partial has no trailing newline
	path := writeFile(t, "line1\npartial")
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if string(lines[0]) != "line1" {
		t.Errorf("unexpected line: %q", lines[0])
	}
	// nextOffset points past "line1\n" only, not including "partial"
	if nextOffset != 6 {
		t.Errorf("expected nextOffset 6, got %d", nextOffset)
	}
}

func TestReadLines_OnlyPartialLine(t *testing.T) {
	// A file with bytes but no newline at all
	path := writeFile(t, "notrailing")
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines (partial only), got %d", len(lines))
	}
	if nextOffset != 0 {
		t.Errorf("expected nextOffset 0, got %d", nextOffset)
	}
}

func TestReadLines_MultipleCompleteLines(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	path := writeFile(t, content)
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"alpha", "beta", "gamma"}
	if len(lines) != len(expected) {
		t.Fatalf("expected %d lines, got %d", len(expected), len(lines))
	}
	for i, want := range expected {
		if string(lines[i]) != want {
			t.Errorf("line[%d]: got %q, want %q", i, lines[i], want)
		}
	}
	if nextOffset != int64(len(content)) {
		t.Errorf("expected nextOffset %d, got %d", len(content), nextOffset)
	}
}

func TestReadLines_MultipleLines_LastPartial(t *testing.T) {
	content := "one\ntwo\nthree\nfour"
	path := writeFile(t, content)
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"one", "two", "three"}
	if len(lines) != len(expected) {
		t.Fatalf("expected %d lines, got %d", len(expected), len(lines))
	}
	// nextOffset points after "one\ntwo\nthree\n" = 14
	if nextOffset != 14 {
		t.Errorf("expected nextOffset 14, got %d", nextOffset)
	}
}

func TestReadLines_IncrementalTailing(t *testing.T) {
	// Simulate cursor-based tailing: first read two lines, then read more.
	path := writeFile(t, "first\nsecond\nthird\n")

	lines1, off1, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("first read error: %v", err)
	}
	if len(lines1) != 3 {
		t.Fatalf("expected 3 lines from full read, got %d", len(lines1))
	}

	// Simulate the file growing: create a new file with more data appended.
	path2 := writeFile(t, "first\nsecond\nthird\nfourth\nfifth\n")

	lines2, off2, err := ReadLines(path2, off1, defaultOpts)
	if err != nil {
		t.Fatalf("incremental read error: %v", err)
	}
	expected := []string{"fourth", "fifth"}
	if len(lines2) != len(expected) {
		t.Fatalf("expected %d new lines, got %d", len(expected), len(lines2))
	}
	for i, want := range expected {
		if string(lines2[i]) != want {
			t.Errorf("incremental line[%d]: got %q, want %q", i, lines2[i], want)
		}
	}
	if off2 != int64(len("first\nsecond\nthird\nfourth\nfifth\n")) {
		t.Errorf("unexpected nextOffset %d", off2)
	}
}

func TestReadLines_OffsetAtEndOfFile(t *testing.T) {
	content := "line\n"
	path := writeFile(t, content)
	// Start reading from the end — nothing to return.
	lines, nextOffset, err := ReadLines(path, int64(len(content)), defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines at EOF offset, got %d", len(lines))
	}
	if nextOffset != int64(len(content)) {
		t.Errorf("expected nextOffset %d, got %d", len(content), nextOffset)
	}
}

func TestReadLines_LineTooLong(t *testing.T) {
	// A line of 10 bytes (excluding newline), cap set to 5.
	content := "0123456789\n"
	path := writeFile(t, content)
	opts := Options{MaxLineSize: 5}
	_, _, err := ReadLines(path, 0, opts)
	if err == nil {
		t.Fatal("expected error for oversized line, got nil")
	}
	if !errors.Is(err, ErrLineTooLong) {
		t.Errorf("expected ErrLineTooLong, got %v", err)
	}
}

func TestReadLines_LineTooLong_OffsetRetained(t *testing.T) {
	// When ErrLineTooLong is returned, nextOffset must equal the original offset
	// so the caller knows no progress was made.
	content := "ok\ntoolongline\n"
	path := writeFile(t, content)
	opts := Options{MaxLineSize: 5}
	_, nextOffset, err := ReadLines(path, 0, opts)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	if nextOffset != 0 {
		t.Errorf("expected nextOffset 0 on error, got %d", nextOffset)
	}
}

func TestReadLines_FileTooLarge(t *testing.T) {
	content := "some content\n"
	path := writeFile(t, content)
	opts := Options{MaxFileSize: 5}
	_, _, err := ReadLines(path, 0, opts)
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("expected ErrFileTooLarge, got %v", err)
	}
}

func TestReadLines_BinaryGarbage(t *testing.T) {
	// Non-UTF-8 bytes should be returned as-is (JSON validation is caller's job).
	garbage := []byte{0xFF, 0xFE, 0x00, 0x01, '\n', 0xDE, 0xAD, '\n'}
	path := writeFileBytes(t, garbage)
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error reading binary content: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines from binary file, got %d", len(lines))
	}
	if string(lines[0]) != string([]byte{0xFF, 0xFE, 0x00, 0x01}) {
		t.Errorf("binary line[0] mismatch")
	}
	if string(lines[1]) != string([]byte{0xDE, 0xAD}) {
		t.Errorf("binary line[1] mismatch")
	}
	if nextOffset != int64(len(garbage)) {
		t.Errorf("expected nextOffset %d, got %d", len(garbage), nextOffset)
	}
}

func TestReadLines_EmptyLines(t *testing.T) {
	// Empty lines (bare newlines) should be returned as empty slices.
	content := "a\n\nb\n"
	path := writeFile(t, content)
	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if string(lines[0]) != "a" {
		t.Errorf("line[0]: got %q", lines[0])
	}
	if len(lines[1]) != 0 {
		t.Errorf("line[1] should be empty, got %q", lines[1])
	}
	if string(lines[2]) != "b" {
		t.Errorf("line[2]: got %q", lines[2])
	}
	if nextOffset != int64(len(content)) {
		t.Errorf("expected nextOffset %d, got %d", len(content), nextOffset)
	}
}

func TestReadLines_LargeFile(t *testing.T) {
	// Generate 100k lines of valid JSONL and verify count and final offset.
	const lineCount = 100_000
	var sb strings.Builder
	lineContent := `{"index":1234567890}` + "\n"
	for i := 0; i < lineCount; i++ {
		sb.WriteString(lineContent)
	}
	content := sb.String()
	path := writeFile(t, content)

	lines, nextOffset, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error on large file: %v", err)
	}
	if len(lines) != lineCount {
		t.Errorf("expected %d lines, got %d", lineCount, len(lines))
	}
	if nextOffset != int64(len(content)) {
		t.Errorf("expected nextOffset %d, got %d", len(content), nextOffset)
	}
}

func TestReadLines_FileNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	_, _, err := ReadLines(path, 0, defaultOpts)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestReadLines_DefaultCaps(t *testing.T) {
	// Zero-value Options should use the built-in defaults without error.
	path := writeFile(t, `{"ok":true}`+"\n")
	lines, _, err := ReadLines(path, 0, Options{})
	if err != nil {
		t.Fatalf("unexpected error with zero Options: %v", err)
	}
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}
}

func TestReadLines_ExactLineCap(t *testing.T) {
	// A line exactly at the cap should succeed; one byte over should fail.
	content := "12345\n"
	path := writeFile(t, content)

	opts := Options{MaxLineSize: 5}
	lines, _, err := ReadLines(path, 0, opts)
	if err != nil {
		t.Fatalf("line at exact cap should not error: %v", err)
	}
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}

	content2 := "123456\n"
	path2 := writeFile(t, content2)
	_, _, err = ReadLines(path2, 0, opts)
	if !errors.Is(err, ErrLineTooLong) {
		t.Errorf("expected ErrLineTooLong for line one byte over cap, got %v", err)
	}
}

func TestReadLines_ReturnedSlicesAreIndependent(t *testing.T) {
	// Verify that each returned []byte is an independent copy, not a slice into
	// a shared backing array that could be mutated.
	path := writeFile(t, "hello\nworld\n")
	lines, _, err := ReadLines(path, 0, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	// Mutate the first slice and verify the second is unaffected.
	lines[0][0] = 'X'
	if lines[1][0] == 'X' {
		t.Error("mutating lines[0] affected lines[1]: slices share backing array")
	}
}
