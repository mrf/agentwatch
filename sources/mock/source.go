// Package mock provides a deterministic mock source for testing agentwatch monitors.
package mock

import (
	"context"
	"sync"

	"github.com/mrf/agentwatch/source"
)

// Source is a controllable mock source for testing.
type Source struct {
	mu      sync.Mutex
	name    string
	handles []source.SessionHandle
	updates map[string][]pendingUpdate
}

type pendingUpdate struct {
	update source.SourceUpdate
	cursor source.Cursor
	err    error
}

// New returns a mock Source with the given name.
func New(name string) *Source {
	return &Source{
		name:    name,
		updates: make(map[string][]pendingUpdate),
	}
}

// Name implements source.Source.
func (s *Source) Name() string { return s.name }

// SetHandles sets the session handles that Discover will return.
func (s *Source) SetHandles(handles []source.SessionHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = make([]source.SessionHandle, len(handles))
	copy(s.handles, handles)
}

// AddUpdate queues an update for the given session ID. Each call to Parse
// consumes one queued update in FIFO order. When no updates remain, Parse
// returns a zero SourceUpdate and the unchanged cursor.
func (s *Source) AddUpdate(sessionID string, u source.SourceUpdate, cursor source.Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates[sessionID] = append(s.updates[sessionID], pendingUpdate{
		update: u,
		cursor: cursor,
	})
}

// Discover implements source.Source.
func (s *Source) Discover(_ context.Context) ([]source.SessionHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]source.SessionHandle, len(s.handles))
	copy(out, s.handles)
	return out, nil
}

// Parse implements source.Source.
func (s *Source) Parse(_ context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue := s.updates[h.ID]
	if len(queue) == 0 {
		return source.SourceUpdate{}, c, nil
	}

	p := queue[0]
	s.updates[h.ID] = queue[1:]
	return p.update, p.cursor, p.err
}
