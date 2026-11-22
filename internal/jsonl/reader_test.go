package jsonl

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates a temp file with the given content and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
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

func TestRead_ExactLineCap(t *testing.T) {
	// A line exactly at the cap is kept; one byte over is skipped.
	path := writeFile(t, "12345\n123456\n")
	res, err := Read(path, 0, Options{MaxLineSize: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 1 || string(res.Lines[0]) != "12345" {
		t.Errorf("lines = %q, want [12345]", res.Lines)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if res.NextOffset != 13 {
		t.Errorf("NextOffset = %d, want 13", res.NextOffset)
	}
}

func TestRead_OversizedLineSkipped(t *testing.T) {
	content := "ok\ntoolongline\nnext\n"
	path := writeFile(t, content)
	res, err := Read(path, 0, Options{MaxLineSize: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 2 || string(res.Lines[0]) != "ok" || string(res.Lines[1]) != "next" {
		t.Errorf("lines = %q, want [ok next]", res.Lines)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if res.NextOffset != int64(len(content)) {
		t.Errorf("NextOffset = %d, want %d", res.NextOffset, len(content))
	}
}

func TestRead_OnlyOversizedLineAdvancesOffset(t *testing.T) {
	content := "toolongline\n"
	path := writeFile(t, content)
	res, err := Read(path, 0, Options{MaxLineSize: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 0 || res.Skipped != 1 || res.NextOffset != int64(len(content)) {
		t.Errorf("got lines=%d skipped=%d next=%d", len(res.Lines), res.Skipped, res.NextOffset)
	}
}

func TestRead_OversizedLargerThanReadBuffer(t *testing.T) {
	// A skipped line far larger than any internal buffer must still be
	// skipped without being returned.
	big := strings.Repeat("x", 300_000)
	content := "a\n" + big + "\nb\n"
	path := writeFile(t, content)
	res, err := Read(path, 0, Options{MaxLineSize: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 2 || string(res.Lines[0]) != "a" || string(res.Lines[1]) != "b" {
		t.Errorf("lines = %d, want [a b]", len(res.Lines))
	}
	if res.Skipped != 1 || res.NextOffset != int64(len(content)) {
		t.Errorf("skipped=%d next=%d, want 1/%d", res.Skipped, res.NextOffset, len(content))
	}
}

func TestRead_UnterminatedOversizedLineWaits(t *testing.T) {
	// An oversized line that has no newline yet is not skipped (it may still
	// be growing): the offset stays at its start.
	content := "ok\ntoolongpartial"
	path := writeFile(t, content)
	res, err := Read(path, 0, Options{MaxLineSize: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 1 || res.Skipped != 0 || res.NextOffset != 3 {
		t.Errorf("got lines=%d skipped=%d next=%d, want 1/0/3", len(res.Lines), res.Skipped, res.NextOffset)
	}
	// Once the newline lands, the line is skipped and the offset advances.
	if err := os.WriteFile(path, []byte(content+"\nz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Read(path, res.NextOffset, Options{MaxLineSize: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Lines) != 1 || string(res.Lines[0]) != "z" || res.Skipped != 1 {
		t.Errorf("got lines=%q skipped=%d", res.Lines, res.Skipped)
	}
}

func TestRead_BudgetConsumedIncrementally(t *testing.T) {
	// 10 lines of 10 bytes each (9 chars + newline), budget of 35 bytes:
	// each call stops after the line that crosses the budget.
	var content strings.Builder
	for i := 0; i < 10; i++ {
		content.WriteString("line-00" + string(rune('0'+i)) + "\n")
	}
	path := writeFile(t, content.String())
	opts := Options{MaxReadBytes: 35}

	var got []string
	var offset int64
	calls := 0
	for offset < int64(content.Len()) {
		calls++
		if calls > 20 {
			t.Fatal("not making progress")
		}
		res, err := Read(path, offset, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.NextOffset <= offset {
			t.Fatalf("no progress: offset %d -> %d", offset, res.NextOffset)
		}
		if len(res.Lines) > 4 {
			t.Errorf("call %d returned %d lines, budget should cap at 4", calls, len(res.Lines))
		}
		for _, l := range res.Lines {
			got = append(got, string(l))
		}
		offset = res.NextOffset
	}
	if len(got) != 10 || got[0] != "line-000" || got[9] != "line-009" {
		t.Errorf("got %d lines %q", len(got), got)
	}
	if calls < 3 {
		t.Errorf("expected multiple calls, got %d", calls)
	}
}

func TestRead_FileLargerThanBudgetIsReadable(t *testing.T) {
	// Regression: a file larger than the old total-size cap used to fail on
	// every poll. Resuming mid-file must work regardless of total size.
	line := strings.Repeat("y", 99) + "\n"
	path := writeFile(t, strings.Repeat(line, 100))
	opts := Options{MaxReadBytes: 1000}
	var offset int64
	total := 0
	for i := 0; i < 50; i++ {
		res, err := Read(path, offset, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		total += len(res.Lines)
		if res.NextOffset == offset {
			break
		}
		offset = res.NextOffset
	}
	if total != 100 {
		t.Errorf("read %d lines, want 100", total)
	}
}

func TestRead_ReportsSkippedAcrossBudgetBoundary(t *testing.T) {
	// An oversized line that straddles the budget is skipped whole in one call.
	content := "a\n" + strings.Repeat("x", 50) + "\nb\n"
	path := writeFile(t, content)
	res, err := Read(path, 0, Options{MaxLineSize: 10, MaxReadBytes: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if !bytes.Equal(res.Lines[0], []byte("a")) {
		t.Errorf("first line = %q", res.Lines[0])
	}
}

func TestRead_DirectoryRejected(t *testing.T) {
	_, _, err := ReadLines(t.TempDir(), 0, defaultOpts)
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("err = %v, want ErrNotRegular", err)
	}
}
