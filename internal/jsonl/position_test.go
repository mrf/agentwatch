package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func readFromOK(t *testing.T, path string, pos Position) (Result, Position, bool) {
	t.Helper()
	res, next, reset, err := ReadFrom(path, pos, defaultOpts)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	return res, next, reset
}

func lineStrings(res Result) []string {
	out := make([]string, len(res.Lines))
	for i := 0; i < len(res.Lines); i++ {
		out[i] = string(res.Lines[i])
	}
	return out
}

func TestReadFrom_FreshThenAppendDoesNotReset(t *testing.T) {
	path := writeFile(t, "a\nb\n")
	res, pos, reset := readFromOK(t, path, Position{})
	if reset {
		t.Error("first read from the zero position must not report a reset")
	}
	if got := strings.Join(lineStrings(res), ","); got != "a,b" {
		t.Fatalf("lines = %q, want a,b", got)
	}
	if pos.Offset != 4 {
		t.Fatalf("offset = %d, want 4", pos.Offset)
	}

	appendFile(t, path, "c\n")
	res, pos, reset = readFromOK(t, path, pos)
	if reset {
		t.Error("append must not report a reset")
	}
	if got := strings.Join(lineStrings(res), ","); got != "c" {
		t.Fatalf("lines = %q, want c", got)
	}
	if pos.Offset != 6 {
		t.Fatalf("offset = %d, want 6", pos.Offset)
	}
}

func TestReadFrom_NoNewDataKeepsPosition(t *testing.T) {
	path := writeFile(t, "a\n")
	_, pos, _ := readFromOK(t, path, Position{})
	res, next, reset := readFromOK(t, path, pos)
	if reset || len(res.Lines) != 0 {
		t.Fatalf("idle read: reset=%v lines=%d", reset, len(res.Lines))
	}
	if next != pos {
		t.Errorf("idle read changed position: %+v -> %+v", pos, next)
	}
}

func TestReadFrom_TruncationResets(t *testing.T) {
	path := writeFile(t, "first\nsecond\nthird\n")
	_, pos, _ := readFromOK(t, path, Position{})

	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, next, reset := readFromOK(t, path, pos)
	if !reset {
		t.Fatal("shrink below the offset must report a reset")
	}
	if got := strings.Join(lineStrings(res), ","); got != "x" {
		t.Fatalf("lines after truncation = %q, want x", got)
	}
	if next.Offset != 2 {
		t.Errorf("offset = %d, want 2", next.Offset)
	}
}

func TestReadFrom_TruncatedToEmptyResets(t *testing.T) {
	path := writeFile(t, "first\n")
	_, pos, _ := readFromOK(t, path, Position{})
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, next, reset := readFromOK(t, path, pos)
	if !reset || len(res.Lines) != 0 || next.Offset != 0 {
		t.Fatalf("reset=%v lines=%d offset=%d, want reset with nothing read", reset, len(res.Lines), next.Offset)
	}
}

// An in-place rewrite to at least the old size keeps the inode; only the
// head hash can tell it apart from an append.
func TestReadFrom_InPlaceRewriteResets(t *testing.T) {
	path := writeFile(t, "aaaa\nbbbb\n")
	_, pos, _ := readFromOK(t, path, Position{})

	if err := os.WriteFile(path, []byte("zzzz\nyyyy\nxxxx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, next, reset := readFromOK(t, path, pos)
	if !reset {
		t.Fatal("rewrite with a different head must report a reset")
	}
	if got := strings.Join(lineStrings(res), ","); got != "zzzz,yyyy,xxxx" {
		t.Fatalf("lines = %q, want the whole new file", got)
	}
	if next.Offset != 15 {
		t.Errorf("offset = %d, want 15", next.Offset)
	}
}

func TestReadFrom_RotationByRenameResets(t *testing.T) {
	path := writeFile(t, "same\n")
	_, pos, _ := readFromOK(t, path, Position{})

	// Same leading bytes, so only the file identity differs on platforms
	// that expose one; the head hash still matches.
	tmp := filepath.Join(filepath.Dir(path), "replacement")
	if err := os.WriteFile(tmp, []byte("same\nnew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	_, _, reset := readFromOK(t, path, pos)
	if hasFileID && !reset {
		t.Fatal("a replaced file must report a reset")
	}
}

func TestReadFrom_LegacyOffsetOnlyChecksSize(t *testing.T) {
	path := writeFile(t, "aaaa\nbbbb\n")

	res, next, reset := readFromOK(t, path, Position{Offset: 5})
	if reset {
		t.Error("legacy offset within the file must not reset")
	}
	if got := strings.Join(lineStrings(res), ","); got != "bbbb" {
		t.Fatalf("lines = %q, want bbbb", got)
	}
	if next.HeadLen == 0 {
		t.Error("next position must carry identity once upgraded")
	}

	_, _, reset = readFromOK(t, path, Position{Offset: 100})
	if !reset {
		t.Error("legacy offset past EOF must reset")
	}
}

func TestReadFrom_HeadGrowsToCap(t *testing.T) {
	long := strings.Repeat("x", maxHeadLen+100) + "\n"
	path := writeFile(t, "a\n")
	_, pos, _ := readFromOK(t, path, Position{})
	if pos.HeadLen != 2 {
		t.Fatalf("head len = %d, want 2", pos.HeadLen)
	}
	appendFile(t, path, long)
	_, pos, reset := readFromOK(t, path, pos)
	if reset {
		t.Fatal("append must not reset")
	}
	if pos.HeadLen != maxHeadLen {
		t.Errorf("head len = %d, want %d", pos.HeadLen, maxHeadLen)
	}
}

func TestPosition_EncodeParseRoundTrip(t *testing.T) {
	path := writeFile(t, "a\nb\n")
	_, pos, _ := readFromOK(t, path, Position{})
	got, err := ParsePosition(pos.Encode())
	if err != nil {
		t.Fatalf("ParsePosition: %v", err)
	}
	if got != pos {
		t.Errorf("round trip = %+v, want %+v", got, pos)
	}
}

func TestParsePosition_Legacy(t *testing.T) {
	cases := map[string]Position{
		"":    {},
		"0":   {},
		"123": {Offset: 123},
	}
	for in, want := range cases {
		got, err := ParsePosition(in)
		if err != nil {
			t.Errorf("ParsePosition(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePosition(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestParsePosition_Corrupt(t *testing.T) {
	for _, in := range []string{"garbage", "{", `{"offset":-1}`, "-5"} {
		if _, err := ParsePosition(in); err == nil {
			t.Errorf("ParsePosition(%q): expected error", in)
		}
	}
}
