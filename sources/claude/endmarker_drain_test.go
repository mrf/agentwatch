package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newDrainSource(t *testing.T, markerDir string) *ClaudeSource {
	t.Helper()
	src, err := New(WithSessionEndDir(markerDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cs, ok := src.(*ClaudeSource)
	if !ok {
		t.Fatalf("New returned %T, want *ClaudeSource", src)
	}
	return cs
}

func TestDrainEndMarkers_LeavesNonMatchingFilesUntouched(t *testing.T) {
	markerDir := t.TempDir()

	// None of these are names the hook writes (<session_id>.json), so none may
	// be read or deleted regardless of content or size.
	untouched := map[string]string{
		"notes.txt":         "not a marker",
		"settings.json.bak": `{"session_id":"abc","reason":"x"}`,
		"has space.json":    `{"session_id":"abc"}`,
		".hidden.json":      "garbage",
		"big.txt":           strings.Repeat("x", defaultMaxEndMarkerSize+1),
		"nodot":             "garbage",
		strings.Repeat("a", maxSessionIDLen+1) + ".json": "{}",
	}
	for name, content := range untouched {
		if err := os.WriteFile(filepath.Join(markerDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(markerDir, "subdir.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	src := newDrainSource(t, markerDir)
	src.drainEndMarkers(time.Now())

	for name, content := range untouched {
		got, err := os.ReadFile(filepath.Join(markerDir, name))
		if err != nil {
			t.Errorf("non-matching file %q was removed or unreadable: %v", name, err)
			continue
		}
		if string(got) != content {
			t.Errorf("non-matching file %q was modified", name)
		}
	}
	if fi, err := os.Stat(filepath.Join(markerDir, "subdir.json")); err != nil || !fi.IsDir() {
		t.Errorf("directory named like a marker was disturbed: %v", err)
	}
	if n := len(src.pendingTerminals); n != 0 {
		t.Errorf("pendingTerminals = %d, want 0", n)
	}
}

func TestDrainEndMarkers_StillDeletesMatchingInvalidMarkers(t *testing.T) {
	markerDir := t.TempDir()
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(markerDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	writeFile("notjson.json", "garbage")
	writeFile("invalid.json", `{"session_id":"bad id!"}`)
	writeFile("oversize.json", strings.Repeat("x", defaultMaxEndMarkerSize+1))

	src := newDrainSource(t, markerDir)
	src.drainEndMarkers(time.Now())

	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("matching-but-invalid markers should be deleted, %d remain", len(entries))
	}
}

func TestDrainEndMarkers_DrainsMoreThan256Markers(t *testing.T) {
	markerDir := t.TempDir()
	const n = 600
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sess-%04d", i)
		writeEndMarker(t, markerDir, id+".json", map[string]string{
			"session_id": id,
			"reason":     "logout",
		})
	}

	src := newDrainSource(t, markerDir)
	src.drainEndMarkers(time.Now())

	if got := len(src.pendingTerminals); got != n {
		t.Errorf("pendingTerminals = %d, want %d", got, n)
	}
	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d marker files left on disk, want 0", len(entries))
	}
}
