package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/source"
)

func oversizedLine() string {
	return strings.Repeat("x", jsonl.DefaultMaxLineSize+1) + "\n"
}

func TestParse_ReportsSkippedLines(t *testing.T) {
	s := newTestSource(t)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, []byte(oversizedLine()+oversizedLine()), 0o644); err != nil {
		t.Fatal(err)
	}
	h := source.SessionHandle{ID: "dddddddd-0000-0000-0000-000000000002", Path: path, Source: "claude"}

	u, c, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.SkippedLines != 2 {
		t.Errorf("SkippedLines = %d, want 2", u.SkippedLines)
	}

	u, _, err = s.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if u.SkippedLines != 0 {
		t.Errorf("SkippedLines on re-poll = %d, want 0", u.SkippedLines)
	}
}

// Oversized lines in a subagent transcript count toward the parent session's
// update.
func TestParse_ReportsSkippedSubagentLines(t *testing.T) {
	root, parentPath := copyLayoutFixture(t)
	s := newTestSource(t)
	h := source.SessionHandle{ID: layoutParentID, Path: parentPath, Source: "claude"}

	_, c, err := s.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}

	appendLine(t, layoutAgentPath(root, layoutAgent1), strings.TrimSuffix(oversizedLine(), "\n"))
	u, _, err := s.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if u.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1", u.SkippedLines)
	}
}
