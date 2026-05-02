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
	removed map[string]struct{}       // IDs removed after retention; enables resumed-from-removed

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

			switch {
			case !existed:
				_, wasRemoved := m.removed[h.ID]
				if wasRemoved {
					delete(m.removed, h.ID)
				}
				state = applyUpdate(session.SessionState{
					ID:        h.ID,
					Source:    h.Source,
					Lifecycle: session.LifecycleActive,
					StartedAt: h.StartedAt,
				}, update, now)
				if wasRemoved {
					lcEvent.Type = session.EventResumed
					lcEvent.From = session.LifecycleTerminal
					lcEvent.To = session.LifecycleActive
				} else {
					lcEvent.Type = session.EventDiscovered
					lcEvent.To = session.LifecycleActive
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

	// Deliver events to sinks outside any lock.
	if m.cfg.sink != nil && len(updates) > 0 {
		m.emitLifecycleEvents(ctx, lifecycles)
		m.emitEvent(ctx, Event{
			Seq:     m.seq.Add(1),
			At:      m.cfg.clock.Now(),
			Type:    EventDelta,
			Updates: updates,
		})
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

// Run starts the monitor loop. It polls sources at the configured interval
// and reaps terminal sessions whose retention window has expired.
// Run blocks until ctx is canceled and returns ctx.Err().
func (m *Monitor) Run(ctx context.Context) error {
	ticker := m.cfg.clock.NewTicker(m.cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C():
			if err := m.PollOnce(ctx); err != nil {
				return err
			}
			m.reapTerminal(ctx)
		}
	}
}

// reapTerminal removes terminal sessions whose retention window has expired
// and emits EventRemoved lifecycle events for each removal.
func (m *Monitor) reapTerminal(ctx context.Context) {
	now := m.cfg.clock.Now()

	m.mu.Lock()
	var removals []session.LifecycleEvent
	for id, state := range m.store {
		if state.Lifecycle != session.LifecycleTerminal {
			continue
		}
		if state.CompletedAt == nil {
			continue
		}
		if now.Sub(*state.CompletedAt) < m.cfg.completionRetention {
			continue
		}
		delete(m.store, id)
		m.removed[id] = struct{}{}
		removals = append(removals, session.LifecycleEvent{
			Type:      session.EventRemoved,
			SessionID: id,
			Source:    state.Source,
			From:      session.LifecycleTerminal,
			At:        now,
			Reason:    "retention expired",
		})
	}
	m.mu.Unlock()

	if m.cfg.sink != nil && len(removals) > 0 {
		m.emitLifecycleEvents(ctx, removals)
	}
}

// emitLifecycleEvents wraps each lifecycle event in an Event envelope and
// delivers it to the sink. Must be called outside store locks.
func (m *Monitor) emitLifecycleEvents(ctx context.Context, events []session.LifecycleEvent) {
	for i := 0; i < len(events); i++ {
		lc := events[i]
		m.emitEvent(ctx, Event{
			Seq:       m.seq.Add(1),
			At:        lc.At,
			Type:      EventLifecycle,
			Lifecycle: &lc,
		})
	}
}

// emitEvent delivers a single event to the sink and logs any error.
func (m *Monitor) emitEvent(ctx context.Context, ev Event) {
	if err := m.cfg.sink.HandleEvent(ctx, ev); err != nil {
		m.cfg.logger.Warn("sink error",
			"type", ev.Type,
			"error", err,
		)
	}
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
