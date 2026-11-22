package claude

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/source"
)

func TestParse_TruncationAndRotationReset(t *testing.T) {
	_, h := writeTranscript(t)
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	testutil.CheckRewriteResets(t, src, h,
		tokUserLine+"\n"+tokAssistantLine+"\n"+tokUserLine+"\n",
		tokAssistantLine+"\n",
	)
}

func TestParse_CorruptCursorSignalsReset(t *testing.T) {
	_, h := writeTranscript(t, tokUserLine, tokAssistantLine)
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	u, c, err := src.Parse(context.Background(), h, source.Cursor("{not json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !u.Reset {
		t.Error("corrupt cursor: Reset = false; the re-read would double-count")
	}
	if u.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta = %d, want 2 (absolute)", u.MessageCountDelta)
	}
	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatal(err)
	}
	if u.Reset || u.MessageCountDelta != 0 {
		t.Errorf("after recovery: Reset=%v msgs=%d, want a plain idle poll", u.Reset, u.MessageCountDelta)
	}
}

func TestParse_LegacyOffsetCursorStillResumes(t *testing.T) {
	_, h := writeTranscript(t, tokUserLine, tokAssistantLine)
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	legacy := source.Cursor(strconv.Itoa(len(tokUserLine) + 1))
	u, _, err := src.Parse(context.Background(), h, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if u.Reset || u.MessageCountDelta != 1 {
		t.Errorf("legacy cursor: Reset=%v msgs=%d, want resume with 1 message", u.Reset, u.MessageCountDelta)
	}
}

func TestParse_SubagentFileTruncationResetsItsState(t *testing.T) {
	root, parent := copyLayoutFixture(t)
	src, err := New(WithRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	h := source.SessionHandle{ID: layoutParentID, Path: parent, Source: "claude"}
	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	before, ok := subagentByID(u.Subagents, layoutAgent1)
	if !ok || before.MessageCount < 2 {
		t.Fatalf("fixture subagent %s: %+v, want at least 2 messages", layoutAgent1, before)
	}

	agentPath := layoutAgentPath(root, layoutAgent1)
	data, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	if err := os.WriteFile(agentPath, []byte(first+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := subagentByID(u.Subagents, layoutAgent1)
	if !ok {
		t.Fatalf("truncated subagent not reported: %+v", u.Subagents)
	}
	if after.MessageCount != 1 {
		t.Errorf("subagent MessageCount after truncation = %d, want 1 (re-read, not accumulated onto %d)", after.MessageCount, before.MessageCount)
	}
}
