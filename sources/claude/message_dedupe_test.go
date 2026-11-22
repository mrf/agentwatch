package claude

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// blockLine is one content-block record of the assistant message id; Claude
// Code writes one such record per block, each repeating message.id.
func blockLine(id, block string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"role":"assistant","model":"claude-opus-4-6","content":[%s],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}},"timestamp":"2026-01-01T10:00:01.000Z","cwd":"/tmp/p","sessionId":"tok-1","gitBranch":"main"}`, id, block)
}

const (
	textBlock = `{"type":"text","text":"ok"}`
	toolBlock = `{"type":"tool_use","name":"Read"}`
)

func TestParseLines_SplitMessageCountsOnce(t *testing.T) {
	r := parseLines([][]byte{
		[]byte(blockLine("msg_A", textBlock)),
		[]byte(blockLine("msg_A", toolBlock)),
		[]byte(blockLine("msg_A", toolBlock)),
		[]byte(blockLine("msg_B", textBlock)),
	})
	if r.msgDelta != 2 {
		t.Errorf("msgDelta = %d, want 2 (msg_A split across 3 records + msg_B)", r.msgDelta)
	}
	if r.toolDelta != 2 {
		t.Errorf("toolDelta = %d, want 2: tool_use blocks still count per record", r.toolDelta)
	}
}

func TestParseLines_AssistantWithoutIDStillCountsEachRecord(t *testing.T) {
	r := parseLines([][]byte{[]byte(tokAssistantLine), []byte(tokAssistantLine)})
	if r.msgDelta != 2 {
		t.Errorf("msgDelta = %d, want 2 (no message.id, nothing to dedupe on)", r.msgDelta)
	}
}

func TestParse_SplitMessageAcrossParseCalls(t *testing.T) {
	path, h := writeTranscript(t, tokUserLine, blockLine("msg_A", textBlock))
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.MessageCountDelta != 2 {
		t.Fatalf("first MessageCountDelta = %d, want 2", u.MessageCountDelta)
	}

	appendTranscript(t, path, blockLine("msg_A", toolBlock), blockLine("msg_B", textBlock))
	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatal(err)
	}
	if u.MessageCountDelta != 1 {
		t.Errorf("second MessageCountDelta = %d, want 1 (msg_A continuation carried via cursor, msg_B new)", u.MessageCountDelta)
	}
}

func TestParse_SubagentSplitMessageAcrossParseCalls(t *testing.T) {
	path, h := writeTranscript(t, tokUserLine)
	dir := strings.TrimSuffix(path, ".jsonl") + "/subagents"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentPath := dir + "/agent-a1.jsonl"
	sidechain := func(id, block string) string {
		return strings.Replace(blockLine(id, block), `"type":"assistant"`, `"isSidechain":true,"type":"assistant"`, 1)
	}
	appendTranscript(t, agentPath, sidechain("msg_S", textBlock))

	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Subagents) != 1 || u.Subagents[0].MessageCount != 1 {
		t.Fatalf("first subagents = %+v, want one with MessageCount 1", u.Subagents)
	}

	appendTranscript(t, agentPath, sidechain("msg_S", toolBlock), sidechain("msg_T", textBlock))
	u, _, err = src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Subagents) != 1 || u.Subagents[0].MessageCount != 2 {
		t.Errorf("second subagents = %+v, want MessageCount 2 (msg_S counted once, msg_T new)", u.Subagents)
	}
}

func TestParse_SplitMessageAcrossParseCallsContinuationOnly(t *testing.T) {
	path, h := writeTranscript(t, blockLine("msg_A", textBlock))
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, path, blockLine("msg_A", toolBlock))
	u, _, err := src.Parse(context.Background(), h, c)
	if err != nil {
		t.Fatal(err)
	}
	if u.MessageCountDelta != 0 {
		t.Errorf("MessageCountDelta = %d, want 0 for a pure continuation of msg_A", u.MessageCountDelta)
	}
	if u.ToolCallCountDelta != 1 {
		t.Errorf("ToolCallCountDelta = %d, want 1: the continuation's tool_use still counts", u.ToolCallCountDelta)
	}
}
