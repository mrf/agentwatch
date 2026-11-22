package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsAgyCmdline(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{"bare name", "agy\x00", true},
		{"absolute path", "/usr/local/bin/agy\x00--flag\x00", true},
		{"editor opening agy file", "vim\x00/home/u/agy\x00", false},
		{"agy as later argument", "less\x00agy\x00", false},
		{"different binary", "agyd\x00", false},
		{"empty", "", false},
	}
	for i := 0; i < len(tests); i++ {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			if got := isAgyCmdline([]byte(tc.cmdline)); got != tc.want {
				t.Errorf("isAgyCmdline(%q) = %v, want %v", tc.cmdline, got, tc.want)
			}
		})
	}
}

func TestDiscover_IgnoresNonUUIDJSON(t *testing.T) {
	root := t.TempDir()
	const id = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	sub := filepath.Join(root, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(root, id+".json"),
		filepath.Join(root, "settings.json"),
		filepath.Join(root, "config.json"),
		filepath.Join(sub, "cache.json"),
		filepath.Join(root, id+"-extra.json"),
	}
	for i := 0; i < len(files); i++ {
		if err := os.WriteFile(files[i], []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(WithRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].ID != id {
		t.Fatalf("got %+v, want exactly session %s", handles, id)
	}
}

func TestDiscover_ProcessScanCachedWithTTL(t *testing.T) {
	root := t.TempDir()
	const id = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if err := os.WriteFile(filepath.Join(root, id+".json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, lastConversationFile), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := New(WithRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := src.(*AntigravitySource)
	if !ok {
		t.Fatalf("New returned %T", src)
	}

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	scans := 0
	s.scanProcs = func() []string {
		scans++
		return []string{"/work/proj"}
	}

	discover := func() string {
		t.Helper()
		h, err := s.Discover(context.Background())
		if err != nil || len(h) != 1 {
			t.Fatalf("Discover: %v, %d handles", err, len(h))
		}
		return h[0].WorkingDir
	}

	if got := discover(); got != "/work/proj" {
		t.Errorf("WorkingDir = %q, want /work/proj", got)
	}
	discover()
	if scans != 1 {
		t.Errorf("scans within TTL = %d, want 1", scans)
	}

	now = now.Add(procCacheTTL + time.Second)
	discover()
	if scans != 2 {
		t.Errorf("scans after TTL = %d, want 2", scans)
	}
}
