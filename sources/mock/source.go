// Package mock provides a deterministic mock source for testing agentwatch monitors.
// It implements source.Source with scripted results, error injection, and call recording.
package mock

import (
	"context"
	"sync"

	"github.com/mrf/agentwatch/source"
)

// DiscoverCall records a single invocation of Discover.
type DiscoverCall struct{}

// ParseCall records a single invocation of Parse, including the arguments.
type ParseCall struct {
	Handle source.SessionHandle
	Cursor source.Cursor
}

// discoverResult is one scripted response for Discover.
type discoverResult struct {
	handles []source.SessionHandle
	err     error
}

// parseResult is one scripted response for Parse.
type parseResult struct {
	update source.SourceUpdate
	cursor source.Cursor
	err    error
}

// Source is a deterministic mock that implements source.Source.
// Construct it with New and configure it with Option functions or the
// Queue* methods for runtime mutations.
type Source struct {
	name string

	mu              sync.Mutex
	discoverQueue   []discoverResult
	discoverCalls   []DiscoverCall
	parseQueues     map[string][]parseResult // keyed by session ID
	parseCalls      []ParseCall
	defaultParse    *parseResult // fallback when a session queue is exhausted
}

// Option configures a Source.
type Option func(*Source)

// New creates a new mock Source with the given options.
func New(opts ...Option) *Source {
	s := &Source{
		name:        "mock",
		parseQueues: make(map[string][]parseResult),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WithName sets the source name returned by Name.
func WithName(name string) Option {
	return func(s *Source) {
		s.name = name
	}
}

// WithHandles queues a Discover result that returns the given handles (no error).
// Multiple calls append results in order.
func WithHandles(handles ...source.SessionHandle) Option {
	return func(s *Source) {
		s.discoverQueue = append(s.discoverQueue, discoverResult{handles: handles})
	}
}

// WithDiscoverError queues a Discover result that returns err.
func WithDiscoverError(err error) Option {
	return func(s *Source) {
		s.discoverQueue = append(s.discoverQueue, discoverResult{err: err})
	}
}

// WithParseResult queues a Parse result for the given sessionID.
// Multiple calls for the same sessionID append results in order.
func WithParseResult(sessionID string, update source.SourceUpdate, cursor source.Cursor, err error) Option {
	return func(s *Source) {
		s.parseQueues[sessionID] = append(s.parseQueues[sessionID], parseResult{
			update: update,
			cursor: cursor,
			err:    err,
		})
	}
}

// WithDefaultParse sets a fallback Parse result used for any session whose
// scripted queue is empty. The default is not consumed — it repeats on every call.
func WithDefaultParse(update source.SourceUpdate, cursor source.Cursor, err error) Option {
	return func(s *Source) {
		s.defaultParse = &parseResult{update: update, cursor: cursor, err: err}
	}
}

// Name implements source.Source.
func (s *Source) Name() string {
	return s.name
}

// Discover implements source.Source. It dequeues and returns the next scripted
// result. When the queue is exhausted it returns nil, nil.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.discoverCalls = append(s.discoverCalls, DiscoverCall{})

	if len(s.discoverQueue) == 0 {
		return nil, nil
	}
	r := s.discoverQueue[0]
	s.discoverQueue = s.discoverQueue[1:]
	return r.handles, r.err
}

// Parse implements source.Source. It dequeues the next scripted result for
// h.ID. If none remains it falls back to the default result, or returns a
// zero SourceUpdate with the incoming cursor unchanged.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.parseCalls = append(s.parseCalls, ParseCall{Handle: h, Cursor: c})

	if q, ok := s.parseQueues[h.ID]; ok && len(q) > 0 {
		r := q[0]
		s.parseQueues[h.ID] = q[1:]
		return r.update, r.cursor, r.err
	}
	if s.defaultParse != nil {
		return s.defaultParse.update, s.defaultParse.cursor, s.defaultParse.err
	}
	return source.SourceUpdate{}, c, nil
}

// DiscoverCallCount returns the number of times Discover has been called.
func (s *Source) DiscoverCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.discoverCalls)
}

// ParseCallCount returns the number of times Parse has been called.
func (s *Source) ParseCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.parseCalls)
}

// ParseCalls returns a snapshot of all recorded Parse invocations.
func (s *Source) ParseCalls() []ParseCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ParseCall, len(s.parseCalls))
	copy(result, s.parseCalls)
	return result
}

// QueueHandles enqueues a Discover result returning the given handles.
// Safe to call after construction (runtime mutation).
func (s *Source) QueueHandles(handles ...source.SessionHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoverQueue = append(s.discoverQueue, discoverResult{handles: handles})
}

// QueueParseResult enqueues a Parse result for sessionID.
// Safe to call after construction (runtime mutation).
func (s *Source) QueueParseResult(sessionID string, update source.SourceUpdate, cursor source.Cursor, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parseQueues[sessionID] = append(s.parseQueues[sessionID], parseResult{
		update: update,
		cursor: cursor,
		err:    err,
	})
}
