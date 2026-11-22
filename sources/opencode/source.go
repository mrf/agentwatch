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
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Source implements source.Source for OpenCode agent sessions.
type Source struct {
	dbPath string
	window time.Duration

	// mu guards db and dbInfo and serializes queries on the single handle.
	mu     sync.Mutex
	db     *sql.DB
	dbInfo os.FileInfo
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

// Close releases the database handle. The Source remains usable: the next
// Discover or Parse reopens it.
func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Source) closeLocked() error {
	db := s.db
	s.db, s.dbInfo = nil, nil
	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("opencode: close db: %w", err)
	}
	return nil
}

// acquireLocked returns the long-lived read-only handle, opening it on first
// use and reopening it if the file was replaced. It returns (nil, nil) while
// the database file does not exist. Callers hold s.mu.
func (s *Source) acquireLocked() (*sql.DB, error) {
	if s.dbPath == "" {
		return nil, nil //nolint:nilnil // (nil, nil) is the documented "no database yet" result
	}
	info, err := os.Stat(s.dbPath)
	if err != nil {
		_ = s.closeLocked()
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // (nil, nil) is the documented "no database yet" result
		}
		return nil, fmt.Errorf("opencode: stat %s: %w", s.dbPath, err)
	}
	if s.db != nil && os.SameFile(s.dbInfo, info) {
		return s.db, nil
	}
	_ = s.closeLocked()
	db, err := openReadOnly(s.dbPath)
	if err != nil {
		return nil, err
	}
	s.db, s.dbInfo = db, info
	return db, nil
}

// Discover implements source.Source. It queries the OpenCode SQLite database
// for sessions, optionally filtering by the discover window.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.acquireLocked()
	if err != nil || db == nil {
		return nil, err
	}

	var cutoff time.Time
	if s.window > 0 {
		cutoff = time.Now().Add(-s.window)
	}

	rows, err := discoverSessions(ctx, db, cutoff)
	if err != nil {
		_ = s.closeLocked()
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

// Parse implements source.Source. The cursor records what was last reported
// (counts, tokens, last activity, archive state); an unchanged session yields
// a zero update so the monitor can let it go stale, and an archived session
// is reported terminal once. A cursor that cannot be decoded is discarded and
// the session is re-read from scratch as a Reset update.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	cur, err := decodeCursor(string(c))
	reset := err != nil
	if reset {
		cur = cursor{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.acquireLocked()
	if err != nil || db == nil {
		return source.SourceUpdate{}, c, err
	}

	result, newCur, err := queryParseData(ctx, db, h.ID, cur)
	if err != nil {
		_ = s.closeLocked()
		return source.SourceUpdate{}, c, err
	}
	if !result.changed {
		return source.SourceUpdate{}, c, nil
	}

	activity := session.ActivityIdle
	switch {
	case result.terminal:
		activity = session.ActivityTerminal
	case result.inProgress, result.lastRole == "user":
		activity = session.ActivityWorking
	case result.replyDone:
		activity = session.ActivityWaiting
	case result.toolDelta > 0, result.msgDelta > 0:
		activity = session.ActivityWorking
	}

	update := source.SourceUpdate{
		SessionID:          h.ID,
		Slug:               result.slug,
		Model:              result.model,
		Activity:           activity,
		HasTokens:          true,
		ContextTokens:      result.tokensInput,
		OutputTokens:       result.tokensOutput,
		MessageCountDelta:  result.msgDelta,
		ToolCallCountDelta: result.toolDelta,
		CurrentTool:        result.currentTool,
		WorkingDir:         result.directory,
		StartedAt:          result.createdAt,
		LastActivityAt:     result.lastActivity,
		Terminal:           result.terminal,
		Reset:              reset,
	}
	if result.terminal {
		update.EndReason = "archived"
		update.EndedAt = result.archivedAt
	}

	return update, source.Cursor(encodeCursor(newCur)), nil
}
