// Package opencode provides the OpenCode agent source for agentwatch.
//
// OpenCode stores sessions in a SQLite database (typically at
// ~/.local/share/opencode/opencode.db). This source queries the database
// read-only to discover and parse session state.
//
// This source does NOT resolve the database path from XDG_DATA_HOME or any
// environment variable. Callers must supply the path explicitly via
// WithDBPath. If no path is provided the source returns no sessions.
package opencode

import (
	"context"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Source implements source.Source for OpenCode agent sessions.
type Source struct {
	dbPath string
	window time.Duration
}

// Option configures a Source.
type Option func(*Source)

// WithDBPath sets the path to the OpenCode SQLite database.
// Without this option the source discovers no sessions.
func WithDBPath(path string) Option {
	return func(s *Source) {
		s.dbPath = path
	}
}

// WithDiscoverWindow limits discovery to sessions whose updated_at is within
// d of the current time. Zero disables age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *Source) {
		s.window = d
	}
}

// New creates a new OpenCode source with the given options.
func New(opts ...Option) *Source {
	s := &Source{}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Register registers this source with a source.Registry using a factory that
// calls New(opts...).
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("opencode", func() (source.Source, error) {
		return New(opts...), nil
	})
}

// Name implements source.Source.
func (s *Source) Name() string { return "opencode" }

// Discover implements source.Source. It queries the OpenCode SQLite database
// for sessions, optionally filtering by the discover window.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.dbPath == "" {
		return nil, nil
	}
	db, err := openReadOnly(s.dbPath)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()

	var cutoff time.Time
	if s.window > 0 {
		cutoff = time.Now().Add(-s.window)
	}

	rows, err := discoverSessions(db, cutoff)
	if err != nil {
		return nil, err
	}

	handles := make([]source.SessionHandle, 0, len(rows))
	for i := 0; i < len(rows); i++ {
		handles = append(handles, source.SessionHandle{
			ID:         rows[i].id,
			Path:       s.dbPath,
			WorkingDir: nullStr(rows[i].directory),
			StartedAt:  parseTimestamp(rows[i].createdAt),
			Source:     "opencode",
		})
	}
	return handles, nil
}

// Parse implements source.Source. The cursor encodes message and tool call
// counts from the previous parse; deltas are computed by subtraction.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	cur, err := decodeCursor(string(c))
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	db, err := openReadOnly(s.dbPath)
	if err != nil {
		return source.SourceUpdate{}, c, err
	}
	if db == nil {
		return source.SourceUpdate{}, c, nil
	}
	defer func() { _ = db.Close() }()

	result, newCur, err := queryParseData(db, h.ID, cur)
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	activity := session.ActivityIdle
	if result.terminal {
		activity = session.ActivityTerminal
	} else if result.toolDelta > 0 {
		activity = session.ActivityWorking
	} else if result.msgDelta > 0 && result.lastRole == "user" {
		activity = session.ActivityWaiting
	} else if result.msgDelta > 0 {
		activity = session.ActivityWorking
	}

	update := source.SourceUpdate{
		SessionID:          h.ID,
		Slug:               result.slug,
		Model:              result.model,
		Activity:           activity,
		ContextTokens:      result.tokensInput,
		OutputTokens:       result.tokensOutput,
		MessageCountDelta:  result.msgDelta,
		ToolCallCountDelta: result.toolDelta,
		CurrentTool:        result.currentTool,
		WorkingDir:         result.directory,
		StartedAt:          result.createdAt,
		LastActivityAt:     result.lastActivity,
		Terminal:           result.terminal,
	}
	if result.terminal {
		update.EndReason = "archived"
		update.EndedAt = result.archivedAt
	}

	return update, source.Cursor(encodeCursor(newCur)), nil
}
