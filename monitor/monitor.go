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
	store   map[string]session.State
	cursors map[string]cursorEntry // "source:sessionID" -> cursor
	removed map[string]struct{}    // session IDs removed by retention sweep
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
		store:        make(map[string]session.State),
		cursors:      make(map[string]cursorEntry),
		removed:      make(map[string]struct{}),
		contentStart: make(map[string]struct{}),
		health:       make(map[string]*sourceHealth),
	}, nil
}

// cursorEntry is a session's opaque cursor plus the bookkeeping
// forgetUndiscovered needs.
type cursorEntry struct {
	cursor   source.Cursor
	source   string
	id       string
	lastSeen time.Time // last poll whose Discover returned the session
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

	b := pollBatch{discovered: make(map[string]struct{})}

	for i := 0; i < len(m.cfg.sources); i++ {
		if err := m.pollSource(ctx, m.cfg.sources[i], &b); err != nil {
			return err
		}
	}

	m.sweepStale(&b)
	m.sweepCompletions(&b)
	m.sweepRetention(&b)
	m.deliver(ctx, &b)
	return nil
}

// pollBatch accumulates one PollOnce's committed changes for delivery after
// every store lock is released.
type pollBatch struct {
	updates      []session.State
	lifecycles   []session.LifecycleEvent
	healthEvents []Health
	completions  []subagentCompletion
	removedIDs   []string
	discovered   map[string]struct{} // sources whose Discover succeeded
}

// pollSource discovers and parses every session of src, then records the
// source's health. It returns ctx.Err() if the context is canceled.
func (m *Monitor) pollSource(ctx context.Context, src source.Source, b *pollBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	srcName := src.Name()
	handles, err := src.Discover(ctx)
	if err != nil {
		m.cfg.logger.Warn("discover failed",
			"source", srcName,
			"error", err,
		)

		if h, changed := m.updateHealth(srcName, func(sh *sourceHealth, threshold int) {
			sh.recordDiscoverFailure(discoverErrorCategory(err), threshold)
		}); changed {
			b.healthEvents = append(b.healthEvents, h)
		}
		return nil
	}
	b.discovered[srcName] = struct{}{}

	parseFailed := make(map[string]struct{})
	skipped := 0
	for j := 0; j < len(handles); j++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		h := handles[j]
		update, ok := m.parseHandle(ctx, src, h)
		if !ok {
			parseFailed[h.ID] = struct{}{}
			continue
		}

		// Counted before the no-data shortcut: a batch of only oversized
		// lines has no SessionID but still happened.
		skipped += update.SkippedLines

		// No new data — zero Update per the source contract.
		if update.SessionID == "" {
			continue
		}
		m.commitUpdate(h, update, b)
	}

	handleIDs := make([]string, 0, len(handles))
	for j := 0; j < len(handles); j++ {
		handleIDs = append(handleIDs, handles[j].ID)
	}
	if h, changed := m.updateHealth(srcName, func(sh *sourceHealth, threshold int) {
		sh.recordDiscoverSuccess(threshold)
		sh.recordParses(handleIDs, parseFailed, threshold)
		sh.skippedLines += skipped
	}); changed {
		b.healthEvents = append(b.healthEvents, h)
	}
	return nil
}

// parseHandle parses h from its stored cursor and persists the returned
// cursor. It reports false, after logging, when the parse failed.
func (m *Monitor) parseHandle(ctx context.Context, src source.Source, h source.SessionHandle) (source.Update, bool) {
	srcName := src.Name()
	cKey := cursorKey(srcName, h.ID)

	m.mu.Lock()
	cursor := m.cursors[cKey].cursor
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
	m.cursors[cKey] = cursorEntry{
		cursor:   newCursor,
		source:   srcName,
		id:       h.ID,
		lastSeen: m.cfg.clock.Now(),
	}
	m.mu.Unlock()

	if parseErr != nil {
		m.cfg.logger.Warn("parse failed",
			"source", srcName,
			"session", h.ID,
			"error", parseErr,
		)
		return source.Update{}, false
	}
	return update, true
}

// commitUpdate applies a non-empty update to the store under m.mu and queues
// the resulting state and lifecycle event on b. Events are delivered after
// unlock (§12.8).
func (m *Monitor) commitUpdate(h source.SessionHandle, update source.Update, b *pollBatch) {
	now := m.cfg.clock.Now()

	m.mu.Lock()

	existing, existed := m.store[h.ID]

	// A terminal marker for a session that is already terminal (e.g.
	// consumed after the stale sweep) is not new activity: no
	// transition in ADR 0005 covers it, so it must not resume.
	if existed && existing.Lifecycle == session.LifecycleTerminal && update.Terminal {
		m.mu.Unlock()
		return
	}

	var state session.State
	var lcEvent session.LifecycleEvent
	if existed {
		_, fromContent := m.contentStart[h.ID]
		state, lcEvent = transitionExisting(h, existing, update, now, fromContent)
	} else {
		_, wasRemoved := m.removed[h.ID]
		if wasRemoved {
			delete(m.removed, h.ID)
		}
		state, lcEvent = transitionNew(h, update, now, wasRemoved)
	}

	m.store[h.ID] = state
	if existed {
		b.completions = appendCompletions(b.completions, h, existing.Subagents, state.Subagents)
	}
	if !update.StartedAt.IsZero() {
		m.contentStart[h.ID] = struct{}{}
	}
	cloned := state.Clone()

	m.mu.Unlock()
	// State is committed and lock is released before event collection.

	b.updates = append(b.updates, cloned)
	b.lifecycles = append(b.lifecycles, lcEvent)
}

// transitionNew builds the state and lifecycle event for a session absent
// from the store: Discovered, or Resumed when a retention sweep removed it.
func transitionNew(h source.SessionHandle, update source.Update, now time.Time, wasRemoved bool) (session.State, session.LifecycleEvent) {
	base := session.State{
		ID:        h.ID,
		Source:    h.Source,
		Lifecycle: session.LifecycleActive,
		StartedAt: h.StartedAt,
	}
	state := applyUpdate(base, update, now, false)
	lcEvent := session.LifecycleEvent{
		Type:      session.EventDiscovered,
		SessionID: h.ID,
		Source:    h.Source,
		To:        session.LifecycleActive,
		At:        now,
	}
	if wasRemoved {
		lcEvent.Type = session.EventResumed
		lcEvent.From = session.LifecycleTerminal
	}
	return state, lcEvent
}

// transitionExisting applies update to a stored session and returns the new
// state with its Resumed, Terminal or Updated lifecycle event.
func transitionExisting(h source.SessionHandle, existing session.State, update source.Update, now time.Time, fromContent bool) (session.State, session.LifecycleEvent) {
	state := applyUpdate(existing, update, now, fromContent)
	lcEvent := session.LifecycleEvent{
		SessionID: h.ID,
		Source:    h.Source,
		At:        now,
	}

	switch {
	case existing.Lifecycle == session.LifecycleTerminal:
		state.Lifecycle = session.LifecycleActive
		state.CompletedAt = nil
		lcEvent.Type = session.EventResumed
		lcEvent.From = session.LifecycleTerminal
		lcEvent.To = session.LifecycleActive

	case update.Terminal:
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
		lcEvent.Type = session.EventUpdated
		lcEvent.From = session.LifecycleActive
		lcEvent.To = session.LifecycleActive
	}
	return state, lcEvent
}

// markTerminal moves the stored session id to terminal at now and queues it
// on b with a lifecycle event of type typ. Must be called with m.mu held.
func (m *Monitor) markTerminal(b *pollBatch, id string, s session.State, now time.Time, typ session.LifecycleEventType, reason string) {
	s.Lifecycle = session.LifecycleTerminal
	completedAt := now
	s.CompletedAt = &completedAt
	m.store[id] = s

	b.updates = append(b.updates, s.Clone())
	b.lifecycles = append(b.lifecycles, session.LifecycleEvent{
		Type:      typ,
		SessionID: id,
		Source:    s.Source,
		From:      session.LifecycleActive,
		To:        session.LifecycleTerminal,
		At:        now,
		Reason:    reason,
	})
}

// sweepStale marks active sessions terminal once no data has arrived for the
// stale threshold.
func (m *Monitor) sweepStale(b *pollBatch) {
	if m.cfg.staleThreshold <= 0 {
		return
	}
	now := m.cfg.clock.Now()
	threshold := now.Add(-m.cfg.staleThreshold)

	m.mu.Lock()
	for id, s := range m.store {
		if s.Lifecycle != session.LifecycleActive {
			continue
		}
		if !s.LastDataReceivedAt.IsZero() && s.LastDataReceivedAt.Before(threshold) {
			m.markTerminal(b, id, s, now, session.EventStale, "no data received past stale threshold")
		}
	}
	m.mu.Unlock()
}

// sweepCompletions marks active sessions terminal when a parent session
// reports the corresponding subagent as completed. Match by slug, within the
// parent's source, and only in the poll where the subagent transitioned to
// completed, so a session that resumes later is not swept again.
func (m *Monitor) sweepCompletions(b *pollBatch) {
	if len(b.completions) == 0 {
		return
	}
	now := m.cfg.clock.Now()
	m.mu.Lock()
	for id, s := range m.store {
		if s.Lifecycle != session.LifecycleActive || s.Slug == "" {
			continue
		}
		if !matchesCompletion(b.completions, id, s) {
			continue
		}
		m.markTerminal(b, id, s, now, session.EventTerminal, "parent reported subagent completion")
	}
	m.mu.Unlock()
}

// sweepRetention removes terminal sessions past the retention window and
// forgets cursors of sessions no source still reports.
func (m *Monitor) sweepRetention(b *pollBatch) {
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
		b.removedIDs = append(b.removedIDs, id)

		b.lifecycles = append(b.lifecycles, session.LifecycleEvent{
			Type:      session.EventRemoved,
			SessionID: id,
			Source:    s.Source,
			From:      session.LifecycleTerminal,
			At:        now,
			Reason:    "retention expired",
		})
	}
	m.forgetUndiscovered(now, b.discovered)
	m.mu.Unlock()
}

// deliver emits b's lifecycle, delta and health events to the sink. Must be
// called outside any lock.
func (m *Monitor) deliver(ctx context.Context, b *pollBatch) {
	if m.cfg.sink == nil {
		return
	}
	m.emitLifecycleEvents(ctx, b.lifecycles)

	if len(b.updates) > 0 || len(b.removedIDs) > 0 {
		m.emitEvent(ctx, Event{
			Seq:     m.seq.Add(1),
			At:      m.cfg.clock.Now(),
			Type:    EventDelta,
			Updates: dedupeUpdates(b.updates),
			Removed: b.removedIDs,
		})
	}

	for i := 0; i < len(b.healthEvents); i++ {
		h := b.healthEvents[i]
		m.emitEvent(ctx, Event{
			Seq:    m.seq.Add(1),
			At:     h.UpdatedAt,
			Type:   EventHealth,
			Health: &h,
		})
	}
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

// forgetUndiscovered drops the cursor and removed-ID entries of sessions that
// are not in the store and that their source has not reported for the
// retention window. Only a source whose Discover succeeded this poll vouches
// for a session's absence. A forgotten session that reappears is re-read from
// a zero cursor into an empty store and reported as Discovered, so nothing is
// counted twice. Must be called with m.mu held.
func (m *Monitor) forgetUndiscovered(now time.Time, discovered map[string]struct{}) {
	forgot := false
	for key, e := range m.cursors {
		if _, ok := discovered[e.source]; !ok {
			continue
		}
		if _, stored := m.store[e.id]; stored {
			continue
		}
		if now.Sub(e.lastSeen) < m.cfg.completionRetention {
			continue
		}
		delete(m.cursors, key)
		forgot = true
	}
	if !forgot {
		return
	}
	// A removed ID must outlive every cursor for it: the resume path relies
	// on it to re-read a revived session from zero.
	live := make(map[string]struct{}, len(m.cursors))
	for _, e := range m.cursors {
		live[e.id] = struct{}{}
	}
	for id := range m.removed {
		if _, ok := live[id]; !ok {
			delete(m.removed, id)
		}
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

// applyUpdate merges an Update into a State.
// Delta fields (MessageCountDelta, ToolCallCountDelta) are accumulated,
// or replace the counts when u.Reset is set.
// Current-state fields are overwritten when the source provides a value.
// startFromContent reports whether state.StartedAt already came from source
// content rather than the Discover-time handle.
func applyUpdate(state session.State, u source.Update, now time.Time, startFromContent bool) session.State {
	overlay(&state.Slug, u.Slug)
	overlay(&state.Activity, u.Activity)
	overlay(&state.Model, u.Model)
	applyTokens(&state, u)
	applyCounts(&state, u)
	overlay(&state.CurrentTool, u.CurrentTool)
	overlay(&state.WorkingDir, u.WorkingDir)
	overlay(&state.Branch, u.Branch)
	state.StartedAt = mergeStartedAt(state.StartedAt, startFromContent, u.StartedAt)
	overlayTime(&state.LastActivityAt, u.LastActivityAt)
	state.LastDataReceivedAt = now

	if !u.Reset && len(u.Subagents) > 0 {
		state.Subagents = mergeSubagents(state.Subagents, u.Subagents)
	}

	return state
}

// applyTokens copies u's token counts into state and recomputes utilization.
func applyTokens(state *session.State, u source.Update) {
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
}

// applyCounts accumulates u's count deltas into state, or replaces the counts
// and subagents when u.Reset is set.
func applyCounts(state *session.State, u source.Update) {
	if u.Reset {
		state.MessageCount = u.MessageCountDelta
		state.ToolCallCount = u.ToolCallCountDelta
		state.CompactionCount = u.CompactionCountDelta
		state.Subagents = mergeSubagents(nil, u.Subagents)
		return
	}
	state.MessageCount += u.MessageCountDelta
	state.ToolCallCount += u.ToolCallCountDelta
	state.CompactionCount += u.CompactionCountDelta
}

// overlay sets *dst to v unless v is the zero value.
func overlay[T comparable](dst *T, v T) {
	var zero T
	if v != zero {
		*dst = v
	}
}

// overlayTime sets *dst to v unless v.IsZero().
func overlayTime(dst *time.Time, v time.Time) {
	if !v.IsZero() {
		*dst = v
	}
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
func matchesCompletion(completions []subagentCompletion, id string, s session.State) bool {
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
	overlay(&dst.ParentID, src.ParentID)
	overlay(&dst.Slug, src.Slug)
	overlay(&dst.Activity, src.Activity)
	overlay(&dst.CurrentTool, src.CurrentTool)
	overlayTime(&dst.StartedAt, src.StartedAt)
	overlayTime(&dst.LastActivityAt, src.LastActivityAt)
	overlay(&dst.Model, src.Model)
	overlay(&dst.ContextTokens, src.ContextTokens)
	overlay(&dst.OutputTokens, src.OutputTokens)
	overlay(&dst.MessageCount, src.MessageCount)
	overlay(&dst.ToolCallCount, src.ToolCallCount)
	overlayTime(&dst.CompletedAt, src.CompletedAt)
	return dst
}

// dedupeUpdates collapses repeated sessions in a delta to one entry per ID.
// A session touched by both the parse loop and a sweep keeps its last state at
// the position of its first appearance.
func dedupeUpdates(updates []session.State) []session.State {
	idx := make(map[string]int, len(updates))
	out := make([]session.State, 0, len(updates))
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
func (m *Monitor) Snapshot() []session.State {
	m.mu.Lock()
	states := make([]session.State, 0, len(m.store))
	for _, s := range m.store {
		states = append(states, s.Clone())
	}
	m.mu.Unlock()
	slices.SortFunc(states, func(a, b session.State) int {
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
func (m *Monitor) SnapshotWithSeq() ([]session.State, uint64) {
	// seq is read before the store: reading it after could claim a Seq whose
	// state the snapshot missed, but never the reverse.
	seq := m.seq.Load()
	return m.Snapshot(), seq
}

// Get returns a deep copy of the session with the given ID and true,
// or a zero State and false if no session with that ID exists.
func (m *Monitor) Get(id string) (session.State, bool) {
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
