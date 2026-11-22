package antigravity

import (
	"os"
	"path/filepath"
	"strings"
)

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
		if !isAgyCmdline(cmdline) {
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

// isAgyCmdline reports whether the NUL-separated cmdline belongs to an
// Antigravity CLI process. Only argv[0] is considered: later arguments such
// as `vim ~/agy` name files, not the running executable.
func isAgyCmdline(cmdline []byte) bool {
	argv0, _, _ := strings.Cut(string(cmdline), "\x00")
	return argv0 != "" && filepath.Base(argv0) == "agy"
}
