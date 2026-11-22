package pi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/sources/pi"
)

func oversizedLine() string {
	return `{"type":"message","pad":"` + strings.Repeat("x", jsonl.DefaultMaxLineSize) + `"}` + "\n"
}

func TestParseReportsSkippedLines(t *testing.T) {
	content := sessionHeaderFixture("skip") + oversizedLine() + oversizedLine()
	root, h := writeSession(t, "skip", content)
	src := pi.New(pi.WithRoot(root))

	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertInt(t, "SkippedLines", u.SkippedLines, 2)

	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	assertInt(t, "SkippedLines on re-poll", u.SkippedLines, 0)
}

// An all-oversized batch has no parsed lines, yet the count must still be
// reported; the update carries no SessionID so it creates no session.
func TestParseReportsSkippedLinesWithoutData(t *testing.T) {
	root, h := writeSession(t, "skip-only", oversizedLine())
	src := pi.New(pi.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertInt(t, "SkippedLines", u.SkippedLines, 1)
	assertEqual(t, "SessionID", u.SessionID, "")
}
