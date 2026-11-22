// Package monitor provides the orchestration API for agentwatch.
// It constructs and runs monitors, exposes snapshots, and delivers events.
package monitor

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Monitor orchestrates source polling, state management, and event delivery.
type Monitor struct {
	cfg config

	// pollSem serializes PollOnce. It is a channel semaphore rather than a
	// mutex so a blocked caller can give up when its context is canceled.
	// It is separate from mu, which is never held across Parse or sinks.
	pollSem chan struct{}

	mu      sync.Mutex
	store   map[string]session.SessionState
	cursors map[string]source.Cursor // "source:sessionID" -> cursor
	removed map[string]struct{}      // session IDs removed by retention sweep
	// contentStart holds IDs whose StartedAt came from source content rather
	// than the Discover-time handle (see mergeStartedAt).
	contentStart map[string]struct{}
	health       map[string]*sourceHealth // keyed by source name

	seq atomic.Uint64
}

// New creates a Monitor with the given options.
// It returns an error if the configuration is invalid.
func New(opts ...Option) (*Monitor, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &Monitor{
		cfg:          cfg,
		pollSem:      make(chan struct{}, 1),
		store:        make(map[string]session.SessionState),
		cursors:      make(map[string]source.Cursor),
		removed:      make(map[string]struct{}),
		contentStart: make(map[string]struct{}),
		health:       make(map[string]*sourceHealth),
	}, nil
}

// cursorKey returns the map key for a session's cursor, scoped by source name.
func cursorKey(sourceName, sessionID string) string {
	return sourceName + ":" + sessionID
}

// PollOnce discovers and parses all sources, updates the internal store,
// and delivers events to sinks. Individual source errors are logged but
// do not stop the poll. Returns ctx.Err() if the context is canceled.
//
// Calls are serialized: a PollOnce that arrives while another (including
// Run's internal poll) is in flight waits for it to finish, so each cursor
// is read, parsed and persisted by one poll at a time and deltas are applied
// once. A waiting call returns ctx.Err() if its context is canceled first.
func (m *Monitor) PollOnce(ctx context.Context) error {
	select {
	case m.pollSem <- struct{}{}:
		defer func() { <-m.pollSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	var (
		updates      []session.SessionState
		lifecycles   []session.LifecycleEvent
		healthEvents []Health
		completions  []subagentCompletion
	)

	for i := 0; i < len(m.cfg.sources); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		src := m.cfg.sources[i]
		srcName := src.Name()
		parseFailed := make(map[string]struct{})
		skipped := 0

		handles, err := src.Discover(ctx)
		if err != nil {
			m.cfg.logger.Warn("discover failed",
				"source", srcName,
				"error", err,
			)

			if h, changed := m.updateHealth(srcName, func(sh *sourceHealth, threshold int) {
				sh.recordDiscoverFailure(discoverErrorCategory(err), threshold)
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

			// A removed session's state is gone but its cursor is kept, so
			// the batch that revives it holds only incremental deltas. Re-read
			// from a zero cursor to rebuild whole-history counts and
			// StartedAt. On a failed re-read keep the pre-resume cursor so
			// the next poll retries the same path.
			if parseErr == nil && update.SessionID != "" && m.isRemoved(h.ID) {
				update, newCursor, parseErr = src.Parse(ctx, h, "")
				if parseErr != nil {
					newCursor = cursor
				}
			}

			// Persist the returned cursor unconditionally: the source
			// contract says a source that wants a retry returns the cursor
			// it was given, so empty batches and errors must not drop an
			// advance (design rule 5: never inspect or compare a Cursor).
			m.mu.Lock()
			m.cursors[cKey] = newCursor
			m.mu.Unlock()

			if parseErr != nil {
				m.cfg.logger.Warn("parse failed",
					"source", srcName,
					"session", h.ID,
					"error", parseErr,
				)
				parseFailed[h.ID] = struct{}{}
				continue
			}

			// Counted before the no-data shortcut: a batch of only oversized
			// lines has no SessionID but still happened.
			skipped += update.SkippedLines

			// No new data — zero SourceUpdate per the source contract.
			if update.SessionID == "" {
				continue
			}

			now := m.cfg.clock.Now()

			// Lock, commit state change, collect event info, then unlock.
			// Events are delivered after unlock (§12.8).
			m.mu.Lock()

			existing, existed := m.store[h.ID]

			// A terminal marker for a session that is already terminal (e.g.
			// consumed after the stale sweep) is not new activity: no
			// transition in ADR 0005 covers it, so it must not resume.
			if existed && existing.Lifecycle == session.LifecycleTerminal && update.Terminal {
				m.mu.Unlock()
				continue
			}

			var state session.SessionState
			lcEvent := session.LifecycleEvent{
				SessionID: h.ID,
				Source:    h.Source,
				At:        now,
			}

			_, wasRemoved := m.removed[h.ID]
			_, fromContent := m.contentStart[h.ID]

			switch {
			case !existed:
				base := session.SessionState{
					ID:        h.ID,
					Source:    h.Source,
					Lifecycle: session.LifecycleActive,
					StartedAt: h.StartedAt,
				}
				state = applyUpdate(base, update, now, false)
				lcEvent.To = session.LifecycleActive

				if wasRemoved {
					delete(m.removed, h.ID)
					lcEvent.Type = session.EventResumed
					lcEvent.From = session.LifecycleTerminal
				} else {
					lcEvent.Type = session.EventDiscovered
				}

			case existing.Lifecycle == session.LifecycleTerminal:
				state = applyUpdate(existing, update, now, fromContent)
				state.Lifecycle = session.LifecycleActive
				state.CompletedAt = nil
				lcEvent.Type = session.EventResumed
				lcEvent.From = session.LifecycleTerminal
				lcEvent.To = session.LifecycleActive

			case update.Terminal:
				state = applyUpdate(existing, update, now, fromContent)
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
				state = applyUpdate(existing, update, now, fromContent)
				lcEvent.Type = session.EventUpdated
				lcEvent.From = session.LifecycleActive
				lcEvent.To = session.LifecycleActive
			}

			m.store[h.ID] = state
			if existed {
				completions = appendCompletions(completions, h, existing.Subagents, state.Subagents)
			}
			if !update.StartedAt.IsZero() {
				m.contentStart[h.ID] = struct{}{}
			}
			cloned := state.Clone()

			m.mu.Unlock()
			// State is committed and lock is released before event collection.

			updates = append(updates, cloned)
			lifecycles = append(lifecycles, lcEvent)
		}

		// Update health for this source after processing all handles.
		handleIDs := make([]string, 0, len(handles))
		for j := 0; j < len(handles); j++ {
			handleIDs = append(handleIDs, handles[j].ID)
		}
		if h, changed := m.updateHealth(srcName, func(sh *sourceHealth, threshold int) {
			sh.recordDiscoverSuccess(threshold)
			sh.recordParses(handleIDs, parseFailed, threshold)
			sh.skippedLines += skipped
		}); changed {
			healthEvents = append(healthEvents, h)
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
	// the corresponding subagent as completed. Match by slug, within the
	// parent's source, and only in the poll where the subagent transitioned
	// to completed, so a session that resumes later is not swept again.
	if len(completions) > 0 {
		now := m.cfg.clock.Now()
		m.mu.Lock()

		for id, s := range m.store {
			if s.Lifecycle != session.LifecycleActive || s.Slug == "" {
				continue
			}
			if !matchesCompletion(completions, id, s) {
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

		m.mu.Unlock()
	}

	// Sweep: remove terminal sessions past retention window.
	var removedIDs []string
	now := m.cfg.clock.Now()

	m.mu.Lock()
	for id, s := range m.store {
		if s.Lifecycle != session.LifecycleTerminal || s.CompletedAt == nil {
			continue
		}
		if now.Sub(*s.CompletedAt) < m.cfg.completionRetention {
			continue
		}
		delete(m.store, id)
		delete(m.contentStart, id)
		m.removed[id] = struct{}{}
		removedIDs = append(removedIDs, id)

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
				Updates: dedupeUpdates(updates),
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
		}
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
		sh = newSourceHealth()
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
		ParseFailures:    len(sh.parse),
		LastError:        sh.lastError,
		SkippedLines:     sh.skippedLines,
		UpdatedAt:        sh.updatedAt,
	}
}

func (m *Monitor) isRemoved(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.removed[id]
	return ok
}

// updateHealth applies fn to the source's health state under m.mu, recomputes
// its status, and returns a snapshot plus whether the status changed.
// Thread-safe.
func (m *Monitor) updateHealth(name string, fn func(sh *sourceHealth, threshold int)) (Health, bool) {
	m.mu.Lock()
	sh := m.getOrCreateHealth(name)
	prev := sh.status
	fn(sh, m.cfg.healthThreshold)
	sh.updatedAt = m.cfg.clock.Now()
	sh.refresh()
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
// Delta fields (MessageCountDelta, ToolCallCountDelta) are accumulated,
// or replace the counts when u.Reset is set.
// Current-state fields are overwritten when the source provides a value.
// startFromContent reports whether state.StartedAt already came from source
// content rather than the Discover-time handle.
func applyUpdate(state session.SessionState, u source.SourceUpdate, now time.Time, startFromContent bool) session.SessionState {
	if u.Slug != "" {
		state.Slug = u.Slug
	}
	if u.Activity != "" {
		state.Activity = u.Activity
	}
	if u.Model != "" {
		state.Model = u.Model
	}

	// Non-zero counts apply without HasTokens so sources written before the
	// flag existed keep working.
	if u.HasTokens || u.ContextTokens != 0 || u.OutputTokens != 0 || u.TokenEstimated {
		state.ContextTokens = u.ContextTokens
		state.OutputTokens = u.OutputTokens
		state.TokenEstimated = u.TokenEstimated
	}
	if u.MaxContextTokens > 0 {
		state.MaxContextTokens = u.MaxContextTokens
	}
	state.ContextUtilization = session.ComputeContextUtilization(
		state.ContextTokens, state.MaxContextTokens,
	)

	if u.Reset {
		state.MessageCount = u.MessageCountDelta
		state.ToolCallCount = u.ToolCallCountDelta
		state.CompactionCount = u.CompactionCountDelta
		state.Subagents = mergeSubagents(nil, u.Subagents)
	} else {
		state.MessageCount += u.MessageCountDelta
		state.ToolCallCount += u.ToolCallCountDelta
		state.CompactionCount += u.CompactionCountDelta
	}

	if u.CurrentTool != "" {
		state.CurrentTool = u.CurrentTool
	}
	if u.WorkingDir != "" {
		state.WorkingDir = u.WorkingDir
	}
	if u.Branch != "" {
		state.Branch = u.Branch
	}
	state.StartedAt = mergeStartedAt(state.StartedAt, startFromContent, u.StartedAt)
	if !u.LastActivityAt.IsZero() {
		state.LastActivityAt = u.LastActivityAt
	}
	state.LastDataReceivedAt = now

	if !u.Reset && len(u.Subagents) > 0 {
		state.Subagents = mergeSubagents(state.Subagents, u.Subagents)
	}

	return state
}

// mergeStartedAt picks the session start time. Sources report the first
// timestamp of each parsed batch, so later batches carry later times; the
// earliest content timestamp wins. The Discover-time handle value is a file
// mtime, not a start time: the first content timestamp always replaces it,
// even when it is later, so an mtime that predates the first record cannot
// pin a wrong start. A zero incoming value changes nothing.
func mergeStartedAt(cur time.Time, curFromContent bool, in time.Time) time.Time {
	if in.IsZero() {
		return cur
	}
	if !curFromContent || cur.IsZero() || in.Before(cur) {
		return in
	}
	return cur
}

// subagentCompletion records that a subagent with Slug transitioned to
// completed in the session host of Source during one poll.
type subagentCompletion struct {
	source string
	host   string
	slug   string
}

// appendCompletions appends one entry per slugged subagent in next that is
// terminal now but was absent or not terminal in prev.
func appendCompletions(dst []subagentCompletion, h source.SessionHandle, prev, next []session.SubagentState) []subagentCompletion {
	wasDone := make(map[string]struct{}, len(prev))
	for i := 0; i < len(prev); i++ {
		if prev[i].Activity == session.ActivityTerminal {
			wasDone[prev[i].ID] = struct{}{}
		}
	}
	for i := 0; i < len(next); i++ {
		sub := next[i]
		if sub.Activity != session.ActivityTerminal || sub.Slug == "" {
			continue
		}
		if _, done := wasDone[sub.ID]; done {
			continue
		}
		dst = append(dst, subagentCompletion{source: h.Source, host: h.ID, slug: sub.Slug})
	}
	return dst
}

// matchesCompletion reports whether s (stored under id) shares a source and
// slug with a completion reported by a different session.
func matchesCompletion(completions []subagentCompletion, id string, s session.SessionState) bool {
	for i := 0; i < len(completions); i++ {
		c := completions[i]
		if c.source == s.Source && c.slug == s.Slug && c.host != id {
			return true
		}
	}
	return false
}

// mergeSubagents merges incoming subagents into existing by ID and returns a
// new slice. Subagents absent from incoming are kept, new IDs are appended in
// incoming order, and for a matching ID only incoming non-zero fields
// overwrite, so a sparse update (e.g. a completion) cannot wipe known state.
func mergeSubagents(existing, incoming []session.SubagentState) []session.SubagentState {
	out := make([]session.SubagentState, len(existing), len(existing)+len(incoming))
	copy(out, existing)
	index := make(map[string]int, len(out))
	for i := 0; i < len(out); i++ {
		index[out[i].ID] = i
	}
	for i := 0; i < len(incoming); i++ {
		in := incoming[i]
		j, ok := index[in.ID]
		if !ok {
			index[in.ID] = len(out)
			out = append(out, in)
			continue
		}
		out[j] = overlaySubagent(out[j], in)
	}
	return out
}

func overlaySubagent(dst, src session.SubagentState) session.SubagentState {
	if src.ParentID != "" {
		dst.ParentID = src.ParentID
	}
	if src.Slug != "" {
		dst.Slug = src.Slug
	}
	if src.Activity != "" {
		dst.Activity = src.Activity
	}
	if src.CurrentTool != "" {
		dst.CurrentTool = src.CurrentTool
	}
	if !src.StartedAt.IsZero() {
		dst.StartedAt = src.StartedAt
	}
	if !src.LastActivityAt.IsZero() {
		dst.LastActivityAt = src.LastActivityAt
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.ContextTokens != 0 {
		dst.ContextTokens = src.ContextTokens
	}
	if src.OutputTokens != 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.MessageCount != 0 {
		dst.MessageCount = src.MessageCount
	}
	if src.ToolCallCount != 0 {
		dst.ToolCallCount = src.ToolCallCount
	}
	if !src.CompletedAt.IsZero() {
		dst.CompletedAt = src.CompletedAt
	}
	return dst
}

// dedupeUpdates collapses repeated sessions in a delta to one entry per ID.
// A session touched by both the parse loop and a sweep keeps its last state at
// the position of its first appearance.
func dedupeUpdates(updates []session.SessionState) []session.SessionState {
	idx := make(map[string]int, len(updates))
	out := make([]session.SessionState, 0, len(updates))
	for i := 0; i < len(updates); i++ {
		u := updates[i]
		if at, ok := idx[u.ID]; ok {
			out[at] = u
			continue
		}
		idx[u.ID] = len(out)
		out = append(out, u)
	}
	return out
}

// Snapshot returns a deep copy of all currently tracked sessions, ordered by
// (Source, ID). The returned slice is safe to read and mutate without
// affecting the monitor.
func (m *Monitor) Snapshot() []session.SessionState {
	m.mu.Lock()
	states := make([]session.SessionState, 0, len(m.store))
	for _, s := range m.store {
		states = append(states, s.Clone())
	}
	m.mu.Unlock()
	slices.SortFunc(states, func(a, b session.SessionState) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.ID, b.ID))
	})
	return states
}

// SnapshotWithSeq returns a deep copy of all tracked sessions together with
// the Seq of the most recently assigned event (0 if none yet).
//
// Every event with Seq <= the returned seq committed its state before that seq
// was assigned, so the sessions already reflect it. Events with a higher Seq
// may or may not be reflected, so a consumer that bootstraps from this
// snapshot must apply only events with Seq > the returned seq.
func (m *Monitor) SnapshotWithSeq() ([]session.SessionState, uint64) {
	// seq is read before the store: reading it after could claim a Seq whose
	// state the snapshot missed, but never the reverse.
	seq := m.seq.Load()
	return m.Snapshot(), seq
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
