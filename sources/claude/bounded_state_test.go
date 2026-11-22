package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrf/agentwatch/source"
)

func writeMarker(t *testing.T, dir, sessionID string) {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"reason":"exit"}`, sessionID)
	if err := os.WriteFile(filepath.Join(dir, sessionID+".json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func pendingCount(s *Source) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingTerminals)
}

// Markers for sessions that are never parsed (e.g. outside the discover
// window) must expire rather than accumulate.
func TestDrainEndMarkers_ChurnKeepsPendingTerminalsBounded(t *testing.T) {
	markerDir := t.TempDir()
	src := newDrainSource(t, markerDir)

	const markers = 200
	step := pendingTerminalTTL / 10
	bound := int(pendingTerminalTTL/step) + 1
	now := time.Now()
	for i := 0; i < markers; i++ {
		writeMarker(t, markerDir, fmt.Sprintf("ghost-%d", i))
		src.drainEndMarkers(now)
		if n := pendingCount(src); n > bound {
			t.Fatalf("after %d markers: pendingTerminals=%d, want <= %d", i+1, n, bound)
		}
		now = now.Add(step)
	}

	src.drainEndMarkers(now.Add(pendingTerminalTTL))
	if n := pendingCount(src); n != 0 {
		t.Fatalf("after TTL with no new markers: pendingTerminals=%d, want 0", n)
	}
}

// A marker still within its TTL is consumed normally.
func TestDrainEndMarkers_FreshMarkerSurvivesExpiry(t *testing.T) {
	markerDir := t.TempDir()
	src := newDrainSource(t, markerDir)
	now := time.Now()

	writeMarker(t, markerDir, "s1")
	src.drainEndMarkers(now)
	src.drainEndMarkers(now.Add(pendingTerminalTTL - time.Second))

	if _, ok := src.consumePendingTerminal("s1"); !ok {
		t.Fatal("marker expired before its TTL")
	}
}

// The cursor's parent map must not keep an entry for every subagent ever
// seen: once a subagent completes its entry is dead weight.
func TestParse_CursorParentsDropCompletedSubagents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	userLine := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"sanitized"}]},"cwd":"/x","sessionId":"s1","timestamp":"2026-01-01T10:00:00.000Z","gitBranch":"main"}`
	if err := os.WriteFile(path, []byte(userLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := source.SessionHandle{ID: "s1", Path: path, Source: "claude"}
	ctx := context.Background()

	_, c, err := src.Parse(ctx, h, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	const subagents = 50
	for i := 0; i < subagents; i++ {
		ts := fmt.Sprintf("2026-01-01T10:%02d:00.000Z", i)
		appendLine(t, path, fmt.Sprintf(`{"type":"progress","toolUseID":"tu-%d","parentToolUseID":"tp-%d","sessionId":"s1","slug":"sub-%d","timestamp":%q,"data":{"type":"agent_progress","message":{"type":"user"}}}`, i, i, i, ts))
		if _, c, err = src.Parse(ctx, h, c); err != nil {
			t.Fatalf("Parse start %d: %v", i, err)
		}
		appendLine(t, path, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tp-%d","content":[]}]},"cwd":"/x","sessionId":"s1","timestamp":%q,"gitBranch":"main"}`, i, ts))
		u, next, err := src.Parse(ctx, h, c)
		if err != nil {
			t.Fatalf("Parse complete %d: %v", i, err)
		}
		c = next
		if sub, ok := subagentByID(u.Subagents, fmt.Sprintf("tu-%d", i)); !ok || sub.CompletedAt.IsZero() {
			t.Fatalf("subagent %d not completed: %+v", i, u.Subagents)
		}

		cc, err := decodeCursor(c)
		if err != nil {
			t.Fatalf("decodeCursor: %v", err)
		}
		if len(cc.Parents) != 0 {
			t.Fatalf("after %d completed subagents: cursor Parents=%v, want empty", i+1, cc.Parents)
		}
	}
}
