package antigravity

import (
	"os"
	"path/filepath"
	"strings"
)

// scanAntigravityProcesses returns a map from session UUID to working
// directory by inspecting running processes and the last-conversation-id file.
//
// On Linux, it reads /proc/<pid>/cmdline and /proc/<pid>/cwd for each
// process whose command line contains "agy". The active session UUID is read
// from <root>/last-conversation-id. If exactly one agy process is running,
// its working directory is associated with the active session UUID.
//
// This function is best-effort: it returns an empty map on errors or on
// non-Linux platforms. Source-private in v1 — do not export.
func scanAntigravityProcesses(root string) map[string]string {
	result := make(map[string]string)

	// Read the last-conversation-id to know which session is active.
	lastID := readLastConversationID(root)
	if lastID == "" {
		return result
	}

	cwds := scanProcFS()
	if len(cwds) == 1 {
		result[lastID] = cwds[0]
	}

	return result
}

// readLastConversationID reads the last-conversation-id file from root and
// returns the trimmed UUID string. Returns empty on any error.
func readLastConversationID(root string) string {
	data, err := os.ReadFile(filepath.Join(root, lastConversationFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// scanProcFS reads /proc to find agy processes and returns their working
// directories.
func scanProcFS() []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var cwds []string
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
		if !containsAgy(cmdline) {
			continue
		}

		cwd, err := os.Readlink(filepath.Join("/proc", name, "cwd"))
		if err != nil {
			continue
		}

		cwds = append(cwds, cwd)
	}

	return cwds
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

// containsAgy reports whether the NUL-separated cmdline slice contains
// a field that looks like an Antigravity CLI invocation.
func containsAgy(cmdline []byte) bool {
	fields := strings.Split(string(cmdline), "\x00")
	for i := 0; i < len(fields); i++ {
		base := filepath.Base(fields[i])
		if base == "agy" {
			return true
		}
	}
	return false
}
