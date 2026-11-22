package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

const (
	tokUserLine      = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"timestamp":"2026-01-01T10:00:00.000Z","cwd":"/tmp/p","sessionId":"tok-1","gitBranch":"main"}`
	tokAssistantLine = `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":50,"output_tokens":7}},"timestamp":"2026-01-01T10:00:01.000Z","cwd":"/tmp/p","sessionId":"tok-1","gitBranch":"main"}`
)

func TestParseLines_HasTokensOnlyWithUsage(t *testing.T) {
	r := parseLines([][]byte{[]byte(tokUserLine)})
	if r.hasTokens {
		t.Error("user-only batch: hasTokens = true, want false")
	}
	r = parseLines([][]byte{[]byte(tokUserLine), []byte(tokAssistantLine)})
	if !r.hasTokens {
		t.Error("batch with assistant usage: hasTokens = false, want true")
	}
}

func writeTranscript(t *testing.T, lines ...string) (string, source.SessionHandle) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tok-1.jsonl")
	appendTranscript(t, path, lines...)
	return path, source.SessionHandle{ID: "tok-1", Path: path, Source: "claude"}
}

func appendTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for i := 0; i < len(lines); i++ {
		if _, err := f.WriteString(lines[i] + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParse_SetsHasTokensAndMaxContext(t *testing.T) {
	_, h := writeTranscript(t, tokUserLine, tokAssistantLine)
	src, err := New()
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if !u.HasTokens || u.ContextTokens != 200 || u.OutputTokens != 7 {
		t.Errorf("tokens: HasTokens=%v ctx=%d out=%d, want true/200/7", u.HasTokens, u.ContextTokens, u.OutputTokens)
	}
	if u.MaxContextTokens <= 0 {
		t.Errorf("MaxContextTokens = %d for known model %q, want > 0", u.MaxContextTokens, u.Model)
	}
}

func TestParse_UnknownModelHasNoMaxContext(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","model":"mystery-model-1","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}},"timestamp":"2026-01-01T10:00:01.000Z","cwd":"/tmp/p","sessionId":"tok-1"}`
	_, h := writeTranscript(t, tokUserLine, line)
	src, _ := New()
	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.MaxContextTokens != 0 {
		t.Errorf("MaxContextTokens = %d for unknown model, want 0", u.MaxContextTokens)
	}
}

func TestParse_WithContextWindowsOverrides(t *testing.T) {
	_, h := writeTranscript(t, tokUserLine, tokAssistantLine)
	src, _ := New(WithContextWindows(map[string]int{"claude-opus-4-6": 123456}))
	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.MaxContextTokens != 123456 {
		t.Errorf("MaxContextTokens = %d, want 123456", u.MaxContextTokens)
	}
}

func TestWithContextWindows_CopiesAndKeepsDefaults(t *testing.T) {
	m := map[string]int{"custom-model": 5000}
	s := &ClaudeSource{}
	WithContextWindows(m)(s)
	m["custom-model"] = 1
	if got := s.contextWindow("custom-model"); got != 5000 {
		t.Errorf("custom-model = %d, want 5000 (map must be copied)", got)
	}
	if got := s.contextWindow("claude-opus-4-6"); got <= 0 {
		t.Errorf("default model lost after override: %d", got)
	}
}

// A batch with no usage record after one with usage must not zero tokens, and
// a Claude session with a known model reports non-zero utilization.
func TestMonitor_ClaudeTokensSurviveBatchWithoutUsage(t *testing.T) {
	path, _ := writeTranscript(t, tokUserLine, tokAssistantLine)
	src, _ := New(WithRoot(filepath.Dir(path)))
	m, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, path, tokUserLine)
	// Ensure the file looks changed to discovery/poll logic.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snap))
	}
	s := snap[0]
	if s.MessageCount != 3 {
		t.Fatalf("MessageCount = %d, want 3 (second batch not parsed)", s.MessageCount)
	}
	if s.ContextTokens != 200 || s.OutputTokens != 7 {
		t.Errorf("tokens = ctx %d out %d, want 200/7", s.ContextTokens, s.OutputTokens)
	}
	if s.ContextUtilization <= 0 {
		t.Errorf("ContextUtilization = %v, want > 0", s.ContextUtilization)
	}
	if s.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want working", s.Activity)
	}
}
