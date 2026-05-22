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
	health  map[string]*sourceHealth  // keyed by source name

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

	// Sweep: mark active sessions as terminal when a parent session reports
	// the corresponding subagent as completed. Match by slug.
	{
		now := m.cfg.clock.Now()
		m.mu.Lock()

		// Collect slugs of terminal subagents from all sessions.
		completedSlugs := make(map[string]struct{})
		for _, s := range m.store {
			for i := 0; i < len(s.Subagents); i++ {
				sub := s.Subagents[i]
				if sub.Activity == session.ActivityTerminal && sub.Slug != "" {
					completedSlugs[sub.Slug] = struct{}{}
				}
			}
		}

		// Mark active sessions whose slug matches a completed subagent.
		if len(completedSlugs) > 0 {
			for id, s := range m.store {
				if s.Lifecycle != session.LifecycleActive {
					continue
				}
				if s.Slug == "" {
					continue
				}
				if _, ok := completedSlugs[s.Slug]; !ok {
					continue
				}
				s.Lifecycle = session.LifecycleTerminal
				completedAt := now
				s.CompletedAt = &completedAt
				m.store[id] = s

				cloned := s.Clone()
				updates = append(updates, cloned)
				lifecycles = append(lifecycles, session.LifecycleEvent{
					Type:      session.EventTerminal,
					SessionID: id,
					Source:    s.Source,
					From:      session.LifecycleActive,
					To:        session.LifecycleTerminal,
					At:        now,
					Reason:    "parent reported subagent completion",
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
			Reason:    "retention expired",
		})
	}
	m.mu.Unlock()

	// Deliver events to sinks outside any lock.
	if m.cfg.sink != nil {
		m.emitLifecycleEvents(ctx, lifecycles)

		if len(updates) > 0 || len(removedIDs) > 0 {
			m.emitEvent(ctx, Event{
				Seq:     m.seq.Add(1),
				At:      m.cfg.clock.Now(),
				Type:    EventDelta,
				Updates: updates,
				Removed: removedIDs,
			})
		}

		for i := 0; i < len(healthEvents); i++ {
			h := healthEvents[i]
			m.emitEvent(ctx, Event{
				Seq:    m.seq.Add(1),
				At:     h.UpdatedAt,
				Type:   EventHealth,
				Health: &h,
			})
		}
	}

	return nil
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

// Get returns a deep copy of the session with the given ID and true,
// or a zero SessionState and false if no session with that ID exists.
func (m *Monitor) Get(id string) (session.SessionState, bool) {
	m.mu.Lock()
	s, ok := m.store[id]
	if ok {
		s = s.Clone()
	}
	m.mu.Unlock()
	return s, ok
}

// Sources returns the names of all configured sources in their registration order.
func (m *Monitor) Sources() []string {
	names := make([]string, len(m.cfg.sources))
	for i := 0; i < len(m.cfg.sources); i++ {
		names[i] = m.cfg.sources[i].Name()
	}
	return names
}
