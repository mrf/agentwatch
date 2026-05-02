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
	cursors map[string]source.Cursor // "source:sessionID" -> cursor
	health  map[string]*sourceHealth // keyed by source name

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
		health:  make(map[string]*sourceHealth),
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
		updates      []session.SessionState
		lifecycles   []session.LifecycleEvent
		healthEvents []Health
	)

	for i := 0; i < len(m.cfg.sources); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		src := m.cfg.sources[i]
		srcName := src.Name()
		parseFailCount := 0

		handles, err := src.Discover(ctx)
		if err != nil {
			m.cfg.logger.Warn("discover failed",
				"source", srcName,
				"error", err,
			)

			if h, changed := m.recordHealthFailure(srcName, func(sh *sourceHealth) {
				sh.discoverFailures++
				sh.lastError = sanitizeError(err.Error())
			}); changed {
				healthEvents = append(healthEvents, h)
			}
			continue
		}

		for j := 0; j < len(handles); j++ {
			if err := ctx.Err(); err != nil {
				return err
			}

			h := handles[j]
			cKey := cursorKey(srcName, h.ID)

			m.mu.Lock()
			cursor := m.cursors[cKey]
			m.mu.Unlock()

			update, newCursor, parseErr := src.Parse(ctx, h, cursor)
			if parseErr != nil {
				m.cfg.logger.Warn("parse failed",
					"source", srcName,
					"session", h.ID,
					"error", parseErr,
				)
				parseFailCount++
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
				state = applyUpdate(session.SessionState{
					ID:        h.ID,
					Source:    h.Source,
					Lifecycle: session.LifecycleActive,
					StartedAt: h.StartedAt,
				}, update, now)
				lcEvent.Type = session.EventDiscovered
				lcEvent.To = session.LifecycleActive

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

		// Update health for this source after processing all handles.
		if parseFailCount > 0 {
			if h, changed := m.recordHealthFailure(srcName, func(sh *sourceHealth) {
				sh.parseFailures += parseFailCount
				sh.lastError = sanitizeError("parse failure")
			}); changed {
				healthEvents = append(healthEvents, h)
			}
		} else {
			if h, changed := m.recordHealthSuccess(srcName); changed {
				healthEvents = append(healthEvents, h)
			}
		}
	}

	// Deliver events to sinks outside any lock.
	if m.cfg.sink != nil {
		if len(updates) > 0 {
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
			}
			if err := m.cfg.sink.HandleEvent(ctx, ev); err != nil {
				m.cfg.logger.Warn("sink error",
					"type", ev.Type,
					"error", err,
				)
			}
		}

		for i := 0; i < len(healthEvents); i++ {
			h := healthEvents[i]
			seq := m.seq.Add(1)
			ev := Event{
				Seq:    seq,
				At:     h.UpdatedAt,
				Type:   EventHealth,
				Health: &h,
			}
			if err := m.cfg.sink.HandleEvent(ctx, ev); err != nil {
				m.cfg.logger.Warn("sink error",
					"type", ev.Type,
					"error", err,
				)
			}
		}
	}

	return nil
}

// getOrCreateHealth returns the sourceHealth for the named source,
// creating it if it doesn't exist. Must be called with m.mu held.
func (m *Monitor) getOrCreateHealth(name string) *sourceHealth {
	sh, ok := m.health[name]
	if !ok {
		sh = &sourceHealth{status: HealthHealthy}
		m.health[name] = sh
	}
	return sh
}

// snapshotHealth returns a Health snapshot from sourceHealth.
// Must be called with m.mu held.
func (m *Monitor) snapshotHealth(name string, sh *sourceHealth) Health {
	return Health{
		Source:           name,
		Status:           sh.status,
		DiscoverFailures: sh.discoverFailures,
		ParseFailures:    sh.parseFailures,
		LastError:        sh.lastError,
		UpdatedAt:        sh.updatedAt,
	}
}

// recordHealthFailure applies fn to the source's health state (fn should
// increment failure counters and set lastError), recomputes the status, and
// returns a snapshot plus whether the status changed. Thread-safe.
func (m *Monitor) recordHealthFailure(name string, fn func(*sourceHealth)) (Health, bool) {
	m.mu.Lock()
	sh := m.getOrCreateHealth(name)
	prev := sh.status
	fn(sh)
	sh.updatedAt = m.cfg.clock.Now()
	sh.status = computeStatus(sh.totalFailures(), m.cfg.healthThreshold)
	changed := sh.status != prev
	snapshot := m.snapshotHealth(name, sh)
	m.mu.Unlock()
	return snapshot, changed
}

// recordHealthSuccess resets a source's health counters after a fully
// successful poll and returns a snapshot plus whether the status changed.
// Thread-safe.
func (m *Monitor) recordHealthSuccess(name string) (Health, bool) {
	m.mu.Lock()
	sh := m.getOrCreateHealth(name)
	prev := sh.status
	sh.discoverFailures = 0
	sh.parseFailures = 0
	sh.lastError = ""
	sh.updatedAt = m.cfg.clock.Now()
	sh.status = computeStatus(sh.totalFailures(), m.cfg.healthThreshold)
	changed := sh.status != prev
	snapshot := m.snapshotHealth(name, sh)
	m.mu.Unlock()
	return snapshot, changed
}

// Health returns the current health state for all sources.
// The returned map is safe to read and mutate without affecting the monitor.
func (m *Monitor) Health() map[string]Health {
	m.mu.Lock()
	result := make(map[string]Health, len(m.health))
	for name, sh := range m.health {
		result[name] = m.snapshotHealth(name, sh)
	}
	m.mu.Unlock()
	return result
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
