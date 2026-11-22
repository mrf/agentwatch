package cursor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/sources/cursor"
)

func oversizedLine() string {
	return `{"role":"user","pad":"` + strings.Repeat("x", jsonl.DefaultMaxLineSize) + `"}` + "\n"
}

func TestParseReportsSkippedLines(t *testing.T) {
	content := `{"role":"user","message":{"content":[]}}` + "\n" + oversizedLine() + oversizedLine()
	h := writeTranscript(t, "home-user-proj", "skip", content)
	src := cursor.New()

	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.SkippedLines != 2 {
		t.Errorf("SkippedLines = %d, want 2", u.SkippedLines)
	}

	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if u.SkippedLines != 0 {
		t.Errorf("SkippedLines on re-poll = %d, want 0", u.SkippedLines)
	}
}

// An all-oversized batch has no parsed lines, yet the count must still be
// reported; the update carries no SessionID so it creates no session.
func TestParseReportsSkippedLinesWithoutData(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "skip-only", oversizedLine())
	src := cursor.New()

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1", u.SkippedLines)
	}
	if u.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", u.SessionID)
	}
}
