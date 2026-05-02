// Package claude provides the Claude Code source for agentwatch.
//
// A ClaudeSource discovers sessions by walking a directory of Claude Code
// project files (typically ~/.claude/projects) and parsing JSONL session logs.
// Use New with WithRoot to configure the root directory; there are no default
// paths — consumers choose the location.
package claude

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/source"
)

// Option configures a ClaudeSource.
type Option func(*ClaudeSource)

// WithRoot sets the root directory to scan for Claude Code session files.
// Discover walks this directory recursively, collecting all *.jsonl files.
// If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *ClaudeSource) {
		s.root = path
	}
}

// ClaudeSource discovers and parses Claude Code JSONL session files.
// The zero value is usable but will not discover any sessions until
// configured with a non-empty root via WithRoot.
type ClaudeSource struct {
	root string
}

// New returns a new ClaudeSource configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &ClaudeSource{}
	for i := 0; i < len(opts); i++ {
		opts[i](s)
	}
	return s, nil
}

// Register adds a ClaudeSource constructor to r under the name "claude".
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("claude", func() (source.Source, error) {
		return New(opts...)
	})
}

// Name returns "claude".
func (s *ClaudeSource) Name() string { return "claude" }

// Discover walks the configured root and returns a SessionHandle for every
// *.jsonl file found. The session ID is derived from the filename (without
// the .jsonl extension). StartedAt is set from the file modification time.
// WorkingDir is populated during Parse, not Discover.
func (s *ClaudeSource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.root == "" {
		return nil, nil
	}

	var handles []source.SessionHandle
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".jsonl")

		var startedAt time.Time
		if fi, ferr := d.Info(); ferr == nil {
			startedAt = fi.ModTime()
		}

		handles = append(handles, source.SessionHandle{
			ID:        id,
			Path:      path,
			Source:    "claude",
			StartedAt: startedAt,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return handles, nil
}

// Parse reads new JSONL lines from the session file starting at cursor c and
// returns an incremental SourceUpdate. The cursor is a decimal byte-offset
// string; an empty cursor starts from the beginning of the file.
//
// If no new data is available, Parse returns a zero SourceUpdate and the
// unchanged cursor.
func (s *ClaudeSource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	if ctx.Err() != nil {
		return source.SourceUpdate{}, c, ctx.Err()
	}

	// A corrupted cursor resets to the beginning of the file rather than
	// returning an error; re-parsing is idempotent and preferable to failing.
	offset, err := parseCursor(c)
	if err != nil {
		offset = 0
	}

	lines, nextOffset, err := jsonl.ReadLines(h.Path, offset, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	if len(lines) == 0 {
		return source.SourceUpdate{}, c, nil
	}

	result := parseLines(lines)
	nextCursor := source.Cursor(strconv.FormatInt(nextOffset, 10))

	if !result.hasData {
		return source.SourceUpdate{}, nextCursor, nil
	}

	sessionID := result.sessionID
	if sessionID == "" {
		sessionID = h.ID
	}

	update := source.SourceUpdate{
		SessionID:          sessionID,
		Slug:               result.slug,
		Activity:           result.activity,
		Model:              result.model,
		ContextTokens:      result.contextTokens,
		OutputTokens:       result.outputTokens,
		MessageCountDelta:  result.msgDelta,
		ToolCallCountDelta: result.toolDelta,
		CurrentTool:        result.currentTool,
		WorkingDir:         result.cwd,
		Branch:             result.branch,
		LastActivityAt:     result.lastActivityAt,
	}
	if !result.startedAt.IsZero() {
		update.StartedAt = result.startedAt
	}

	return update, nextCursor, nil
}

// parseCursor converts the opaque cursor string to a byte offset.
// An empty cursor returns offset 0.
func parseCursor(c source.Cursor) (int64, error) {
	if c == "" {
		return 0, nil
	}
	return strconv.ParseInt(string(c), 10, 64)
}
