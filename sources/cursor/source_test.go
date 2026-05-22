package cursor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/cursor"
)

// ---- Name ------------------------------------------------------------------

func TestSourceName(t *testing.T) {
	src := cursor.New()
	if src.Name() != "cursor" {
		t.Errorf("Name() = %q, want %q", src.Name(), "cursor")
	}
}

// ---- Discover --------------------------------------------------------------

func TestDiscoverNoRoot(t *testing.T) {
	src := cursor.New()
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverMissingProjectsDir(t *testing.T) {
	root := t.TempDir() // has no "projects/" subdirectory
	src := cursor.New(cursor.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles, got %d", len(handles))
	}
}

func TestDiscoverFlatLayout(t *testing.T) {
	root := t.TempDir()
	transcriptDir := filepath.Join(root, "projects", "home-user-myapp", "agent-transcripts")
	mustMkdir(t, transcriptDir)

	transcriptFile := filepath.Join(transcriptDir, "abc123.jsonl")
	mustWriteFile(t, transcriptFile, flatFixture)

	src := cursor.New(cursor.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(handles))
	}

	h := handles[0]
	if h.Source != "cursor" {
		t.Errorf("Source = %q, want %q", h.Source, "cursor")
	}
	if h.ID != "abc123" {
		t.Errorf("ID = %q, want %q", h.ID, "abc123")
	}
	if h.Path != transcriptFile {
		t.Errorf("Path = %q, want %q", h.Path, transcriptFile)
	}
	if h.WorkingDir != "/home/user/myapp" {
		t.Errorf("WorkingDir = %q, want %q", h.WorkingDir, "/home/user/myapp")
	}
}

func TestDiscoverNestedLayout(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "projects", "home-user-code", "agent-transcripts", "sess001")
	mustMkdir(t, sessionDir)

	transcriptFile := filepath.Join(sessionDir, "sess001.jsonl")
	mustWriteFile(t, transcriptFile, flatFixture)

	src := cursor.New(cursor.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle, got %d", len(handles))
	}

	h := handles[0]
	if h.ID != "sess001" {
		t.Errorf("ID = %q, want %q", h.ID, "sess001")
	}
	if h.WorkingDir != "/home/user/code" {
		t.Errorf("WorkingDir = %q, want %q", h.WorkingDir, "/home/user/code")
	}
}

func TestDiscoverBothLayouts(t *testing.T) {
	root := t.TempDir()

	// Flat layout.
	flatDir := filepath.Join(root, "projects", "home-user-proj1", "agent-transcripts")
	mustMkdir(t, flatDir)
	mustWriteFile(t, filepath.Join(flatDir, "flat001.jsonl"), flatFixture)

	// Nested layout.
	nestedDir := filepath.Join(root, "projects", "home-user-proj2", "agent-transcripts", "nest001")
	mustMkdir(t, nestedDir)
	mustWriteFile(t, filepath.Join(nestedDir, "nest001.jsonl"), flatFixture)

	src := cursor.New(cursor.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("expected 2 handles, got %d", len(handles))
	}
}

func TestDiscoverIgnoresFilesOutsideAgentTranscripts(t *testing.T) {
	root := t.TempDir()

	// File not under agent-transcripts/ — should be ignored.
	otherDir := filepath.Join(root, "projects", "home-user-proj", "other-dir")
	mustMkdir(t, otherDir)
	mustWriteFile(t, filepath.Join(otherDir, "notranscript.jsonl"), `{}`)

	// Valid transcript.
	atDir := filepath.Join(root, "projects", "home-user-proj", "agent-transcripts")
	mustMkdir(t, atDir)
	mustWriteFile(t, filepath.Join(atDir, "valid.jsonl"), flatFixture)

	src := cursor.New(cursor.WithRoot(root))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("expected 1 handle (only the transcript), got %d", len(handles))
	}
	if handles[0].ID != "valid" {
		t.Errorf("ID = %q, want %q", handles[0].ID, "valid")
	}
}

func TestDiscoverWindowFiltersOldFiles(t *testing.T) {
	root := t.TempDir()
	transcriptDir := filepath.Join(root, "projects", "home-user-proj", "agent-transcripts")
	mustMkdir(t, transcriptDir)

	old := filepath.Join(transcriptDir, "oldtranscript.jsonl")
	mustWriteFile(t, old, flatFixture)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	src := cursor.New(cursor.WithRoot(root), cursor.WithDiscoverWindow(24*time.Hour))
	handles, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected 0 handles after age filter, got %d", len(handles))
	}
}

// ---- Register --------------------------------------------------------------

func TestRegister(t *testing.T) {
	r := source.NewRegistry()
	root := t.TempDir()
	if err := cursor.Register(r, cursor.WithRoot(root)); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "cursor" {
		t.Errorf("Names() = %v, want [cursor]", names)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	r := source.NewRegistry()
	if err := cursor.Register(r); err != nil {
		t.Fatalf("first Register error: %v", err)
	}
	if err := cursor.Register(r); err == nil {
		t.Error("expected error on duplicate Register, got nil")
	}
}

// ---- Parse -----------------------------------------------------------------

func TestParseCursorAdvances(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "sess001", flatFixture)
	src := cursor.New()

	_, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if c1 == "" {
		t.Error("cursor should not be empty after reading content")
	}

	// Second parse with cursor should yield no new data and same cursor.
	_, c2, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse error on second call: %v", err)
	}
	if c2 != c1 {
		t.Errorf("cursor changed on re-parse: %v vs %v", c2, c1)
	}
}

func TestParseInvalidCursor(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "sess001", flatFixture)
	src := cursor.New()

	_, _, err := src.Parse(context.Background(), h, source.Cursor("not-a-number"))
	if err == nil {
		t.Error("expected error for invalid cursor")
	}
}

func TestParseEmptyFile(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "empty001", "")
	src := cursor.New()

	u, c, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u.MessageCountDelta != 0 {
		t.Errorf("MessageCountDelta = %d, want 0", u.MessageCountDelta)
	}
	if c != "0" {
		t.Errorf("cursor = %q, want %q", c, "0")
	}
}

func TestParseNormalTranscript(t *testing.T) {
	h := writeTranscript(t, "home-user-myapp", "sess-abc", flatFixture)
	src := cursor.New()

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// flatFixture has 2 lines: user + assistant → 2 messages.
	if u.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta = %d, want 2", u.MessageCountDelta)
	}
	// Last role is assistant → working.
	if u.Activity != session.ActivityWorking {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWorking)
	}
	// WorkingDir comes from handle.
	if u.WorkingDir != "/home/user/myapp" {
		t.Errorf("WorkingDir = %q, want %q", u.WorkingDir, "/home/user/myapp")
	}
	// SessionID from handle.
	if u.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", u.SessionID, "sess-abc")
	}
}

func TestParseMalformedLinesSkipped(t *testing.T) {
	content := "not json\n" +
		`{"role":"user","message":{"content":[]}}` + "\n" +
		"{broken}\n" +
		`{"role":"assistant","message":{"content":[]}}` + "\n"

	h := writeTranscript(t, "home-user-proj", "malformed", content)
	src := cursor.New()

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Only the 2 valid role lines count.
	if u.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta = %d, want 2", u.MessageCountDelta)
	}
}

func TestParseOnlyUserMessages(t *testing.T) {
	content := `{"role":"user","message":{"content":[]}}` + "\n" +
		`{"role":"user","message":{"content":[]}}` + "\n"

	h := writeTranscript(t, "home-user-proj", "useronly", content)
	src := cursor.New()

	u, _, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if u.Activity != session.ActivityWaiting {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWaiting)
	}
	if u.MessageCountDelta != 2 {
		t.Errorf("MessageCountDelta = %d, want 2", u.MessageCountDelta)
	}
}

// ---- fixtures --------------------------------------------------------------

// flatFixture is a minimal two-line transcript: one user message, one
// assistant response.
const flatFixture = `{"role":"user","message":{"content":[{"type":"text","text":"fix the bug"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"I'll take a look"}]}}
`

// ---- helpers ---------------------------------------------------------------

// writeTranscript creates a flat-layout transcript under a temp root and
// returns a SessionHandle with WorkingDir decoded from encodedProject.
func writeTranscript(t *testing.T, encodedProject, id, content string) source.SessionHandle {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", encodedProject, "agent-transcripts")
	mustMkdir(t, dir)
	path := filepath.Join(dir, id+".jsonl")
	mustWriteFile(t, path, content)
	return source.SessionHandle{
		ID:         id,
		Path:       path,
		WorkingDir: "/" + strings.ReplaceAll(encodedProject, "-", "/"),
		Source:     "cursor",
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
