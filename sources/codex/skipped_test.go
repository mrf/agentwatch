package codex_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/sources/codex"
)

func oversizedLine() string {
	return `{"type":"response_item","payload":{"text":"` + strings.Repeat("x", jsonl.DefaultMaxLineSize) + `"}}` + "\n"
}

func TestParseReportsSkippedLines(t *testing.T) {
	normal := `{"type":"session_meta","payload":{"id":"skip-test","model":"o3"}}` + "\n"
	root, h := writeRollout(t, "01234567-abcd-ef01-2345-67890abcdef0", normal+oversizedLine()+oversizedLine())
	src := codex.New(codex.WithRoot(root))

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

// TestMonitorHealthShowsSkippedLines drives a real source through a real
// monitor: the oversized line is visible in Health and the next poll, with
// nothing new skipped, leaves the total unchanged.
func TestMonitorHealthShowsSkippedLines(t *testing.T) {
	root, h := writeRollout(t, liveSessionID, liveBatch1+oversizedLine())
	m, err := monitor.New(monitor.WithSources(codex.New(codex.WithRoot(root))))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := m.Health()["codex"].SkippedLines; got != 1 {
			t.Errorf("poll %d: Health.SkippedLines = %d, want 1", i+1, got)
		}
	}

	appendFile(t, h.Path, oversizedLine())
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Health()["codex"].SkippedLines; got != 2 {
		t.Errorf("after append: Health.SkippedLines = %d, want 2", got)
	}
}

// An all-oversized batch has no parsed lines, yet the count must still be
// reported; the update carries no SessionID so it creates no session.
func TestParseReportsSkippedLinesWithoutData(t *testing.T) {
	root, h := writeRollout(t, "01234567-abcd-ef01-2345-67890abcdef1", oversizedLine())
	src := codex.New(codex.WithRoot(root))

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertInt(t, "SkippedLines", u.SkippedLines, 1)
	assertEqual(t, "SessionID", u.SessionID, "")
}
