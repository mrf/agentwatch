// Package jsonl provides shared types and iteration utilities for Claude Code
// JSONL session files.
package jsonl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

const (
	// MaxFileSize is the maximum JSONL file size ForEachEntry will process.
	// Files exceeding this limit are skipped to prevent OOM from runaway logs.
	MaxFileSize = 100 * 1024 * 1024

	// MaxLineSize is the maximum byte length of a single JSONL line (including
	// the newline terminator). Lines exceeding this limit are skipped.
	MaxLineSize = 1024 * 1024
)

// TokenUsage represents API token usage from an assistant message.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// TotalContext returns the total context tokens (input + cache read + cache creation).
func (t TokenUsage) TotalContext() int {
	return t.InputTokens + t.CacheCreationInputTokens + t.CacheReadInputTokens
}

// Entry is the top-level structure of a Claude Code JSONL line.
type Entry struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	UUID      string          `json:"uuid"`
	SessionID string          `json:"sessionId"`
	Slug      string          `json:"slug"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

// ParseTimestamp parses the entry's RFC3339Nano timestamp.
func (e *Entry) ParseTimestamp() (time.Time, bool) {
	if e.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// MessageContent is the message object inside assistant/user entries.
type MessageContent struct {
	Model   string          `json:"model"`
	Role    string          `json:"role"`
	Usage   *TokenUsage     `json:"usage,omitempty"`
	Content json.RawMessage `json:"content"`
}

// ContentBlock is a single block inside a message's content array.
type ContentBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use block ID
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result references the tool_use
	Text      string          `json:"text,omitempty"`        // text content block
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use input
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result content
}

// ProgressEntry is the top-level structure for type:"progress" JSONL entries.
type ProgressEntry struct {
	Type            string          `json:"type"`
	ToolUseID       string          `json:"toolUseID"`
	ParentToolUseID string          `json:"parentToolUseID"`
	SessionID       string          `json:"sessionId"`
	Slug            string          `json:"slug"`
	Timestamp       string          `json:"timestamp"`
	Data            json.RawMessage `json:"data"`
}

// ProgressDataHeader is used for fast type-checking of progress data.
type ProgressDataHeader struct {
	Type string `json:"type"`
}

// ProgressData wraps the nested data.message structure inside a progress entry.
type ProgressData struct {
	Message struct {
		Type    string          `json:"type"` // "assistant" or "user"
		Message json.RawMessage `json:"message"`
	} `json:"message"`
}

// EntryVisitor is called for each parsed JSONL entry. Return false to stop
// iteration. The line parameter contains the raw bytes of the line without
// the newline terminator.
type EntryVisitor func(entry *Entry, line []byte) bool

// ForEachEntry reads a JSONL file from offset, calling visitor for each
// complete, parseable line. Returns the final byte offset after the last
// complete line consumed.
//
// Files exceeding MaxFileSize are rejected with an error. Lines exceeding
// MaxLineSize are skipped with a warning log and parsing continues.
func ForEachEntry(path string, offset int64, visitor EntryVisitor) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() > MaxFileSize {
		slog.Warn("skipping oversized file", "pkg", "agentwatch/jsonl", "path", path, "size", info.Size(), "limit", MaxFileSize)
		return offset, fmt.Errorf("file size %d exceeds max %d", info.Size(), MaxFileSize)
	}

	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return offset, err
		}
	}

	reader := bufio.NewReader(f)
	parsedOffset := offset

	for {
		line, readErr := reader.ReadBytes('\n')

		if readErr != nil && readErr != io.EOF {
			return parsedOffset, readErr
		}

		if len(line) == 0 {
			break
		}

		// Only process complete lines (ending with newline).
		if line[len(line)-1] != '\n' {
			if readErr == io.EOF {
				break
			}
			continue
		}

		// Skip oversized lines.
		if len(line) > MaxLineSize {
			slog.Warn("skipping oversized line", "pkg", "agentwatch/jsonl", "path", path, "bytes", len(line), "offset", parsedOffset)
			parsedOffset += int64(len(line))
			if readErr == io.EOF {
				break
			}
			continue
		}

		lineData := line[:len(line)-1]

		var entry Entry
		if jsonErr := json.Unmarshal(lineData, &entry); jsonErr != nil {
			parsedOffset += int64(len(line))
			if readErr == io.EOF {
				break
			}
			continue
		}

		parsedOffset += int64(len(line))

		if !visitor(&entry, lineData) {
			break
		}

		if readErr == io.EOF {
			break
		}
	}

	return parsedOffset, nil
}
