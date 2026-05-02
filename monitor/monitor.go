// Package monitor provides the orchestration API for agentwatch.
// It constructs and runs monitors, exposes snapshots, and delivers events.
package monitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Monitor orchestrates source polling, state management, and event delivery.
type Monitor struct {
	cfg config

	mu      sync.Mutex
	store   map[string]session.SessionState
	cursors map[string]source.Cursor  // "source:sessionID" -> cursor
	removed map[string]struct{}       // session IDs removed by retention sweep

	seq atomic.Uint64
}

// New creates a Monitor with the given options.
// It returns an error if the configuration is invalid.
func New(opts ...Option) (*Monitor, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if len(cfg.sources) == 0 {
		return nil, errors.New("monitor: at least one source is required")
	}

	return &Monitor{
		cfg:     cfg,
		store:   make(map[string]session.SessionState),
		cursors: make(map[string]source.Cursor),
		removed: make(map[string]struct{}),
	}, nil
}

// cursorKey returns the map key for a session's cursor, scoped by source name.
func cursorKey(sourceName, sessionID string) string {
	return sourceName + ":" + sessionID
}

// PollOnce discovers and parses all sources, updates the internal store,
// and delivers events to sinks. Individual source errors are logged but
// do not stop the poll. Returns ctx.Err() if the context is canceled.
func (m *Monitor) PollOnce(ctx context.Context) error {
	var (
		updates    []session.SessionState
		lifecycles []session.LifecycleEvent
	)

	for i := 0; i < len(m.cfg.sources); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		src := m.cfg.sources[i]

		handles, err := src.Discover(ctx)
		if err != nil {
			m.cfg.logger.Warn("discover failed",
				"source", src.Name(),
				"error", err,
			)
			continue
		}

		for j := 0; j < len(handles); j++ {
			if err := ctx.Err(); err != nil {
				return err
			}

			h := handles[j]
			cKey := cursorKey(src.Name(), h.ID)

			m.mu.Lock()
			cursor := m.cursors[cKey]
			m.mu.Unlock()

			update, newCursor, parseErr := src.Parse(ctx, h, cursor)
			if parseErr != nil {
				m.cfg.logger.Warn("parse failed",
					"source", src.Name(),
					"session", h.ID,
					"error", parseErr,
				)
				continue
			}

			// No new data — zero SourceUpdate per the source contract.
			if update.SessionID == "" {
				continue
			}

			now := m.cfg.clock.Now()

			// Lock, commit state change, collect event info, then unlock.
			// Events are delivered after unlock (§12.8).
			m.mu.Lock()

			m.cursors[cKey] = newCursor
			existing, existed := m.store[h.ID]

			var state session.SessionState
			lcEvent := session.LifecycleEvent{
				SessionID: h.ID,
				Source:    h.Source,
				At:        now,
			}

			_, wasRemoved := m.removed[h.ID]

			switch {
			case !existed:
				base := session.SessionState{
					ID:        h.ID,
					Source:    h.Source,
					Lifecycle: session.LifecycleActive,
					StartedAt: h.StartedAt,
				}
				state = applyUpdate(base, update, now)
				lcEvent.To = session.LifecycleActive

				if wasRemoved {
					delete(m.removed, h.ID)
					lcEvent.Type = session.EventResumed
					lcEvent.From = session.LifecycleTerminal
				} else {
					lcEvent.Type = session.EventDiscovered
				}

			case existing.Lifecycle == session.LifecycleTerminal:
				state = applyUpdate(existing, update, now)
				state.Lifecycle = session.LifecycleActive
				state.CompletedAt = nil
				lcEvent.Type = session.EventResumed
				lcEvent.From = session.LifecycleTerminal
				lcEvent.To = session.LifecycleActive

			case update.Terminal:
				state = applyUpdate(existing, update, now)
				state.Lifecycle = session.LifecycleTerminal
				endedAt := update.EndedAt
				if endedAt.IsZero() {
					endedAt = now
				}
				state.CompletedAt = &endedAt
				lcEvent.Type = session.EventTerminal
				lcEvent.From = session.LifecycleActive
				lcEvent.To = session.LifecycleTerminal
				lcEvent.Reason = update.EndReason

			default:
				state = applyUpdate(existing, update, now)
				lcEvent.Type = session.EventUpdated
				lcEvent.From = session.LifecycleActive
				lcEvent.To = session.LifecycleActive
			}

			m.store[h.ID] = state
			cloned := state.Clone()

			m.mu.Unlock()
			// State is committed and lock is released before event collection.

			updates = append(updates, cloned)
			lifecycles = append(lifecycles, lcEvent)
		}
	}

	// Sweep: detect stale active sessions.
	if m.cfg.staleThreshold > 0 {
		now := m.cfg.clock.Now()
		threshold := now.Add(-m.cfg.staleThreshold)

		m.mu.Lock()
		for id, s := range m.store {
			if s.Lifecycle != session.LifecycleActive {
				continue
			}
			if !s.LastDataReceivedAt.IsZero() && s.LastDataReceivedAt.Before(threshold) {
				s.Lifecycle = session.LifecycleTerminal
				completedAt := now
				s.CompletedAt = &completedAt
				m.store[id] = s

				cloned := s.Clone()
				updates = append(updates, cloned)
				lifecycles = append(lifecycles, session.LifecycleEvent{
					Type:      session.EventStale,
					SessionID: id,
					Source:    s.Source,
					From:      session.LifecycleActive,
					To:        session.LifecycleTerminal,
					At:        now,
					Reason:    "no data received past stale threshold",
				})
			}
		}
		m.mu.Unlock()
	}

	// Sweep: remove terminal sessions past retention window.
	var removedIDs []string
	now := m.cfg.clock.Now()

	m.mu.Lock()
	for id, s := range m.store {
		if s.Lifecycle != session.LifecycleTerminal {
			continue
		}
		if s.CompletedAt == nil {
			continue
		}
		if now.Sub(*s.CompletedAt) >= m.cfg.completionRetention {
			removedIDs = append(removedIDs, id)
		}
	}
	for i := 0; i < len(removedIDs); i++ {
		id := removedIDs[i]
		s := m.store[id]
		delete(m.store, id)
		m.removed[id] = struct{}{}

		lifecycles = append(lifecycles, session.LifecycleEvent{
			Type:      session.EventRemoved,
			SessionID: id,
			Source:    s.Source,
			From:      session.LifecycleTerminal,
			At:        now,
			Reason:    "retention window expired",
		})
	}
	m.mu.Unlock()

	// Deliver events to sinks outside any lock.
	if m.cfg.sink != nil && (len(updates) > 0 || len(removedIDs) > 0) {
		for i := 0; i < len(lifecycles); i++ {
			lc := lifecycles[i]
			seq := m.seq.Add(1)
			ev := Event{
				Seq:       seq,
				At:        lc.At,
				Type:      EventLifecycle,
				Lifecycle: &lc,
			}
			if err := m.cfg.sink.HandleEvent(ctx, ev); err != nil {
				m.cfg.logger.Warn("sink error",
					"type", ev.Type,
					"error", err,
				)
			}
		}

		seq := m.seq.Add(1)
		ev := Event{
			Seq:     seq,
			At:      m.cfg.clock.Now(),
			Type:    EventDelta,
			Updates: updates,
			Removed: removedIDs,
		}
		if err := m.cfg.sink.HandleEvent(ctx, ev); err != nil {
			m.cfg.logger.Warn("sink error",
				"type", ev.Type,
				"error", err,
			)
		}
	}

	return nil
}

// applyUpdate merges a SourceUpdate into a SessionState.
// Delta fields (MessageCountDelta, ToolCallCountDelta) are accumulated.
// Current-state fields are overwritten when the source provides a value.
func applyUpdate(state session.SessionState, u source.SourceUpdate, now time.Time) session.SessionState {
	if u.Slug != "" {
		state.Slug = u.Slug
	}
	if u.Activity != "" {
		state.Activity = u.Activity
	}
	if u.Model != "" {
		state.Model = u.Model
	}

	state.ContextTokens = u.ContextTokens
	state.OutputTokens = u.OutputTokens
	state.TokenEstimated = u.TokenEstimated
	if u.MaxContextTokens > 0 {
		state.MaxContextTokens = u.MaxContextTokens
	}
	state.ContextUtilization = session.ComputeContextUtilization(
		state.ContextTokens, state.MaxContextTokens,
	)

	state.MessageCount += u.MessageCountDelta
	state.ToolCallCount += u.ToolCallCountDelta

	if u.CurrentTool != "" {
		state.CurrentTool = u.CurrentTool
	}
	if u.WorkingDir != "" {
		state.WorkingDir = u.WorkingDir
	}
	if u.Branch != "" {
		state.Branch = u.Branch
	}
	if !u.StartedAt.IsZero() {
		state.StartedAt = u.StartedAt
	}
	if !u.LastActivityAt.IsZero() {
		state.LastActivityAt = u.LastActivityAt
	}
	state.LastDataReceivedAt = now

	if len(u.Subagents) > 0 {
		state.Subagents = make([]session.SubagentState, len(u.Subagents))
		copy(state.Subagents, u.Subagents)
	}

	return state
}

// Snapshot returns a deep copy of all currently tracked sessions.
// The returned slice is safe to read and mutate without affecting the monitor.
func (m *Monitor) Snapshot() []session.SessionState {
	m.mu.Lock()
	states := make([]session.SessionState, 0, len(m.store))
	for _, s := range m.store {
		states = append(states, s.Clone())
	}
	m.mu.Unlock()
	return states
}
