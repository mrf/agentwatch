package pi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
)

// fixtureLines reads a testdata fixture and returns its lines as raw byte slices,
// matching the representation that jsonl.ReadLines would return.
func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := make([]byte, i-start)
			copy(line, data[start:i])
			lines = append(lines, line)
			start = i + 1
		}
	}
	return lines
}

// runLines feeds lines into a fresh accumulator and returns it.
func runLines(lines [][]byte) *accumulator {
	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], i == 0, &acc)
	}
	return &acc
}

// ---- fixture: simple session -----------------------------------------------

func TestParseLinesSimpleSession(t *testing.T) {
	lines := fixtureLines(t, "simple.jsonl")
	acc := runLines(lines)

	if acc.sessionID != "abcd1234" {
		t.Errorf("sessionID: got %q, want %q", acc.sessionID, "abcd1234")
	}
	if acc.workingDir != "/home/user/project" {
		t.Errorf("workingDir: got %q, want %q", acc.workingDir, "/home/user/project")
	}
	if acc.model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", acc.model, "claude-opus-4-6")
	}
	// contextTokens = input(150) + cacheRead(0) + cacheWrite(0) = 150
	if acc.contextTokens != 150 {
		t.Errorf("contextTokens: got %d, want 150", acc.contextTokens)
	}
	if acc.outputTokens != 25 {
		t.Errorf("outputTokens: got %d, want 25", acc.outputTokens)
	}
	// 1 user + 1 assistant
	if acc.messageDelta != 2 {
		t.Errorf("messageDelta: got %d, want 2", acc.messageDelta)
	}
	if acc.toolCallDelta != 0 {
		t.Errorf("toolCallDelta: got %d, want 0", acc.toolCallDelta)
	}
	// last message is assistant → ActivityWorking
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWorking)
	}
	wantStart, _ := time.Parse(time.RFC3339, "2024-01-15T10:00:00Z")
	if !acc.startedAt.Equal(wantStart) {
		t.Errorf("startedAt: got %v, want %v", acc.startedAt, wantStart)
	}
	wantLast, _ := time.Parse(time.RFC3339, "2024-01-15T10:00:10Z")
	if !acc.lastActivityAt.Equal(wantLast) {
		t.Errorf("lastActivityAt: got %v, want %v", acc.lastActivityAt, wantLast)
	}
}

// ---- fixture: tool session -------------------------------------------------

func TestParseLinesToolSession(t *testing.T) {
	lines := fixtureLines(t, "tool_session.jsonl")
	acc := runLines(lines)

	if acc.sessionID != "efgh5678" {
		t.Errorf("sessionID: got %q, want %q", acc.sessionID, "efgh5678")
	}
	if acc.workingDir != "/home/user/myapp" {
		t.Errorf("workingDir: got %q, want %q", acc.workingDir, "/home/user/myapp")
	}
	// 1 user + 1 assistant = 2
	if acc.messageDelta != 2 {
		t.Errorf("messageDelta: got %d, want 2", acc.messageDelta)
	}
	// 1 toolResult + 1 bashExecution = 2
	if acc.toolCallDelta != 2 {
		t.Errorf("toolCallDelta: got %d, want 2", acc.toolCallDelta)
	}
	// last tool was bashExecution
	if acc.currentTool != "Bash" {
		t.Errorf("currentTool: got %q, want %q", acc.currentTool, "Bash")
	}
	// contextTokens = input(200) + cacheRead(10) + cacheWrite(5) = 215
	if acc.contextTokens != 215 {
		t.Errorf("contextTokens: got %d, want 215", acc.contextTokens)
	}
	if acc.outputTokens != 30 {
		t.Errorf("outputTokens: got %d, want 30", acc.outputTokens)
	}
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWorking)
	}
}

// ---- fixture: model change -------------------------------------------------

func TestParseLinesModelChange(t *testing.T) {
	lines := fixtureLines(t, "model_change.jsonl")
	acc := runLines(lines)

	if acc.sessionID != "ijkl9012" {
		t.Errorf("sessionID: got %q, want %q", acc.sessionID, "ijkl9012")
	}
	// model_change overrides the initial model from the assistant message
	if acc.model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", acc.model, "claude-opus-4-6")
	}
	// last entry is a user message → ActivityWaiting
	if acc.activity != session.ActivityWaiting {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWaiting)
	}
	// 1 assistant + 1 user = 2
	if acc.messageDelta != 2 {
		t.Errorf("messageDelta: got %d, want 2", acc.messageDelta)
	}
}

// ---- fixture: malformed data -----------------------------------------------

func TestParseLinesMalformed(t *testing.T) {
	lines := fixtureLines(t, "malformed.jsonl")
	acc := runLines(lines)

	// Session header should still be parsed despite invalid lines.
	if acc.sessionID != "mnop3456" {
		t.Errorf("sessionID: got %q, want %q", acc.sessionID, "mnop3456")
	}
	if acc.workingDir != "/home/user/project" {
		t.Errorf("workingDir: got %q, want %q", acc.workingDir, "/home/user/project")
	}
	// Only the valid user message counts; compaction/label are no-ops.
	if acc.messageDelta != 1 {
		t.Errorf("messageDelta: got %d, want 1", acc.messageDelta)
	}
	// Invalid JSON lines must not cause a panic or error.
	if acc.toolCallDelta != 0 {
		t.Errorf("toolCallDelta: got %d, want 0", acc.toolCallDelta)
	}
}

// ---- inline unit tests -----------------------------------------------------

func TestParseLineSessionHeader(t *testing.T) {
	line := []byte(`{"type":"session","id":"sess001","version":3,"timestamp":"2024-06-01T09:00:00Z","workingDir":"/tmp/proj"}`)
	var acc accumulator
	parseLine(line, true, &acc)

	if acc.sessionID != "sess001" {
		t.Errorf("sessionID: got %q, want %q", acc.sessionID, "sess001")
	}
	if acc.workingDir != "/tmp/proj" {
		t.Errorf("workingDir: got %q, want %q", acc.workingDir, "/tmp/proj")
	}
	wantT, _ := time.Parse(time.RFC3339, "2024-06-01T09:00:00Z")
	if !acc.startedAt.Equal(wantT) {
		t.Errorf("startedAt: got %v, want %v", acc.startedAt, wantT)
	}
}

func TestParseLineUserMessageSetsWaiting(t *testing.T) {
	line := []byte(`{"type":"message","id":"m1","parentId":"s1","timestamp":"2024-06-01T09:01:00Z","message":{"role":"user","content":"hello"}}`)
	var acc accumulator
	parseLine(line, false, &acc)

	if acc.messageDelta != 1 {
		t.Errorf("messageDelta: got %d, want 1", acc.messageDelta)
	}
	if acc.activity != session.ActivityWaiting {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWaiting)
	}
}

func TestParseLineAssistantMessageSetsWorking(t *testing.T) {
	line := []byte(`{"type":"message","id":"m2","parentId":"m1","timestamp":"2024-06-01T09:02:00Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":500,"output":100,"cacheRead":50,"cacheWrite":20},"stopReason":"end_turn"}}`)
	var acc accumulator
	parseLine(line, false, &acc)

	if acc.messageDelta != 1 {
		t.Errorf("messageDelta: got %d, want 1", acc.messageDelta)
	}
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWorking)
	}
	if acc.model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", acc.model, "claude-opus-4-6")
	}
	// contextTokens = 500 + 50 + 20 = 570
	if acc.contextTokens != 570 {
		t.Errorf("contextTokens: got %d, want 570", acc.contextTokens)
	}
	if acc.outputTokens != 100 {
		t.Errorf("outputTokens: got %d, want 100", acc.outputTokens)
	}
}

func TestParseLineToolResult(t *testing.T) {
	line := []byte(`{"type":"message","id":"m3","parentId":"m2","timestamp":"2024-06-01T09:03:00Z","message":{"role":"toolResult","toolCallId":"call_xyz","toolName":"read_file","content":"file contents","isError":false}}`)
	var acc accumulator
	parseLine(line, false, &acc)

	if acc.toolCallDelta != 1 {
		t.Errorf("toolCallDelta: got %d, want 1", acc.toolCallDelta)
	}
	if acc.currentTool != "read_file" {
		t.Errorf("currentTool: got %q, want %q", acc.currentTool, "read_file")
	}
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWorking)
	}
}

func TestParseLineBashExecution(t *testing.T) {
	line := []byte(`{"type":"message","id":"m4","parentId":"m3","timestamp":"2024-06-01T09:04:00Z","message":{"role":"bashExecution","command":"ls -la","output":"total 8\n","exitCode":0}}`)
	var acc accumulator
	parseLine(line, false, &acc)

	if acc.toolCallDelta != 1 {
		t.Errorf("toolCallDelta: got %d, want 1", acc.toolCallDelta)
	}
	if acc.currentTool != "Bash" {
		t.Errorf("currentTool: got %q, want %q", acc.currentTool, "Bash")
	}
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity: got %q, want %q", acc.activity, session.ActivityWorking)
	}
}

func TestParseLineModelChange(t *testing.T) {
	line := []byte(`{"type":"model_change","id":"mc1","parentId":"m1","timestamp":"2024-06-01T09:05:00Z","provider":"anthropic","modelId":"claude-haiku-4-5"}`)
	var acc accumulator
	acc.model = "claude-3-5-sonnet-20241022" // previously set
	parseLine(line, false, &acc)

	if acc.model != "claude-haiku-4-5" {
		t.Errorf("model: got %q, want %q", acc.model, "claude-haiku-4-5")
	}
}

func TestParseLineMalformedIsSkipped(t *testing.T) {
	line := []byte(`not json at all`)
	var acc accumulator
	acc.sessionID = "original"
	parseLine(line, false, &acc)

	// Malformed line must not overwrite existing accumulator state.
	if acc.sessionID != "original" {
		t.Errorf("sessionID changed by malformed line: got %q", acc.sessionID)
	}
}

func TestParseLineCompactionIsNoOp(t *testing.T) {
	line := []byte(`{"type":"compaction","id":"c1","parentId":"s1","timestamp":"2024-06-01T09:06:00Z","tokensBefore":8000}`)
	var acc accumulator
	parseLine(line, false, &acc)

	// Compaction should not affect counts or activity.
	if acc.messageDelta != 0 {
		t.Errorf("messageDelta: got %d, want 0", acc.messageDelta)
	}
	if acc.toolCallDelta != 0 {
		t.Errorf("toolCallDelta: got %d, want 0", acc.toolCallDelta)
	}
}

func TestParseLineLabelIsNoOp(t *testing.T) {
	line := []byte(`{"type":"label","id":"l1","parentId":"s1","timestamp":"2024-06-01T09:07:00Z"}`)
	var acc accumulator
	parseLine(line, false, &acc)

	if acc.messageDelta != 0 {
		t.Errorf("messageDelta: got %d, want 0", acc.messageDelta)
	}
}

func TestParseLineTimestampTracking(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session","id":"s1","timestamp":"2024-06-01T09:00:00Z","workingDir":"/proj"}`),
		[]byte(`{"type":"message","id":"m1","parentId":"s1","timestamp":"2024-06-01T09:01:00Z","message":{"role":"user","content":"hi"}}`),
		[]byte(`{"type":"message","id":"m2","parentId":"m1","timestamp":"2024-06-01T09:02:30Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":10,"output":5}}}`),
	}
	acc := runLines(lines)

	wantStart, _ := time.Parse(time.RFC3339, "2024-06-01T09:00:00Z")
	if !acc.startedAt.Equal(wantStart) {
		t.Errorf("startedAt: got %v, want %v", acc.startedAt, wantStart)
	}
	wantLast, _ := time.Parse(time.RFC3339, "2024-06-01T09:02:30Z")
	if !acc.lastActivityAt.Equal(wantLast) {
		t.Errorf("lastActivityAt: got %v, want %v", acc.lastActivityAt, wantLast)
	}
}
