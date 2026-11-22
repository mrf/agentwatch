// Package jsonl provides shared types and iteration utilities for Claude Code
// JSONL session files.
package jsonl

import (
	"bufio"
	"encoding/json"
	"errors"
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

// ErrFileTooLarge is returned by ForEachEntry for a file larger than
// MaxFileSize. Retrying will not help until the file shrinks.
var ErrFileTooLarge = errors.New("jsonl: file too large")

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
// Files exceeding MaxFileSize are rejected with ErrFileTooLarge. Lines exceeding
// MaxLineSize are skipped with a warning log and parsing continues.
func ForEachEntry(path string, offset int64, visitor EntryVisitor) (int64, error) {
	f, err := openAt(path, offset)
	if err != nil {
		return offset, err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	parsedOffset := offset

	for {
		line, readErr := reader.ReadBytes('\n')

		if readErr != nil && readErr != io.EOF {
			return parsedOffset, fmt.Errorf("jsonl: read: %w", readErr)
		}

		if len(line) == 0 {
			break
		}

		consumed, stop := visitLine(path, line, parsedOffset, visitor)
		parsedOffset += consumed
		if stop || readErr == io.EOF {
			break
		}
	}

	return parsedOffset, nil
}

// openAt opens path, rejects it if it exceeds MaxFileSize, and seeks to offset.
func openAt(path string, offset int64) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("jsonl: stat: %w", err)
	}
	if info.Size() > MaxFileSize {
		_ = f.Close()
		slog.Warn("skipping oversized file", "pkg", "agentwatch/jsonl", "path", path, "size", info.Size(), "limit", MaxFileSize)
		return nil, fmt.Errorf("%w: size %d exceeds max %d", ErrFileTooLarge, info.Size(), MaxFileSize)
	}

	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("jsonl: seek: %w", err)
		}
	}
	return f, nil
}

// visitLine handles one line read at offset. It returns the bytes consumed
// (zero for an incomplete trailing line, which is re-read once complete) and
// whether visitor asked to stop. Oversized and unparseable lines are consumed
// without visiting.
func visitLine(path string, line []byte, offset int64, visitor EntryVisitor) (consumed int64, stop bool) {
	// Only process complete lines (ending with newline).
	if line[len(line)-1] != '\n' {
		return 0, false
	}

	// Skip oversized lines.
	if len(line) > MaxLineSize {
		slog.Warn("skipping oversized line", "pkg", "agentwatch/jsonl", "path", path, "bytes", len(line), "offset", offset)
		return int64(len(line)), false
	}

	lineData := line[:len(line)-1]

	var entry Entry
	if jsonErr := json.Unmarshal(lineData, &entry); jsonErr != nil {
		return int64(len(line)), false
	}

	return int64(len(line)), !visitor(&entry, lineData)
}
