package gemini

import (
	"crypto/md5" //nolint:gosec // MD5 used only for matching Gemini CLI's own hashing scheme
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// scanGeminiProcesses returns a map from Gemini session hash to working
// directory by inspecting running processes on the host.
//
// On Linux, it reads /proc/<pid>/cmdline and /proc/<pid>/cwd for each
// process whose command line contains "gemini". The working directory is
// hashed with MD5 (Gemini CLI's own scheme) to produce the lookup key.
//
// This function is best-effort: it returns an empty map on errors or on
// non-Linux platforms. Source-private in v1 — do not export.
func scanGeminiProcesses() map[string]string {
	result := make(map[string]string)
	scanProcFS(result)
	return result
}

// scanProcFS reads /proc to find gemini processes and populate hashToDir.
func scanProcFS(hashToDir map[string]string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}

	for i := 0; i < len(entries); i++ {
		de := entries[i]
		if !de.IsDir() {
			continue
		}
		// Only numeric entries are process directories.
		name := de.Name()
		if !isNumeric(name) {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue
		}
		// cmdline is NUL-separated; check if any arg contains "gemini".
		if !containsGemini(cmdline) {
			continue
		}

		cwd, err := os.Readlink(filepath.Join("/proc", name, "cwd"))
		if err != nil {
			continue
		}

		hash := geminiDirHash(cwd)
		hashToDir[hash] = cwd
	}
}

// geminiDirHash computes the directory hash used by Gemini CLI to name
// session directories. Gemini CLI uses the lowercase hex MD5 of the
// absolute working directory path.
func geminiDirHash(dir string) string {
	h := md5.Sum([]byte(dir)) //nolint:gosec
	return hex.EncodeToString(h[:])
}

// isNumeric reports whether s consists entirely of ASCII digits.
func isNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// containsGemini reports whether the NUL-separated cmdline slice contains
// a field that looks like a gemini CLI invocation.
func containsGemini(cmdline []byte) bool {
	fields := strings.Split(string(cmdline), "\x00")
	for i := 0; i < len(fields); i++ {
		base := filepath.Base(fields[i])
		if base == "gemini" || base == "gemini-cli" {
			return true
		}
	}
	return false
}
