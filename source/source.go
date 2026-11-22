// Package source defines the custom source contract for agentwatch.
// Sources implement the Source interface to feed session data into a Monitor.
package source

import (
	"context"
	"time"

	"github.com/mrf/agentwatch/session"
)

// Cursor is an opaque position marker returned by Parse. The monitor stores
// and returns it to the source on the next call but never inspects it.
// Sources may encode byte offsets, mtimes, hashes, or any other
// source-specific continuation token as a string.
type Cursor string

// Source is the interface implemented by all session data providers.
type Source interface {
	// Name returns the unique identifier for this source (e.g. "claude", "codex").
	Name() string

	// Discover returns the set of sessions currently visible to this source.
	// It is called periodically by the monitor to detect new sessions.
	Discover(ctx context.Context) ([]SessionHandle, error)

	// Parse reads new data for the session identified by h, starting from
	// cursor c. It returns the parsed update, the next cursor position, and
	// any error. If no new data is available, it returns a zero Update
	// and the same cursor.
	//
	// The monitor persists the returned cursor on every call — with data,
	// with a zero Update, and alongside a non-nil error — and never
	// inspects or compares it. The cursor is therefore the source's only way
	// to control re-reads:
	//   - To consume input without reporting it (progress-only batches, an
	//     unparseable file), return the advanced cursor. A zero Update
	//     does not create or refresh a session.
	//   - To retry the same input on the next poll (a transient failure),
	//     return the cursor you were given, c, along with the error.
	Parse(ctx context.Context, h SessionHandle, c Cursor) (Update, Cursor, error)
}

// SessionHandle is a lightweight descriptor returned by Discover.
// It identifies a single agent session without exposing parsing state.
type SessionHandle struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	WorkingDir string    `json:"workingDir"`
	StartedAt  time.Time `json:"startedAt"`
	Source     string    `json:"source"`
}

// Update carries incremental session data returned by Parse.
// Delta fields (MessageCountDelta, ToolCallCountDelta) represent the change
// since the previous Parse call; the monitor accumulates them (see Reset).
type Update struct {
	SessionID string `json:"sessionId"`
	Slug      string `json:"slug,omitempty"`

	Activity session.Activity `json:"activity"`
	Model    string           `json:"model,omitempty"`

	// HasTokens marks ContextTokens, OutputTokens and TokenEstimated as
	// reported by this update. When it is false and all three are zero, the
	// monitor keeps the session's previous token counts instead of zeroing
	// them. A source that parsed usage must set it, which also lets it report
	// a genuine zero.
	HasTokens        bool `json:"hasTokens,omitempty"`
	ContextTokens    int  `json:"contextTokens"`
	OutputTokens     int  `json:"outputTokens,omitempty"`
	MaxContextTokens int  `json:"maxContextTokens"`
	TokenEstimated   bool `json:"tokenEstimated"`

	// Reset reports that the source discarded what it had parsed before and
	// started over from the beginning of the session's data, for example
	// because its file was truncated or replaced or its cursor was
	// unreadable. The three count deltas are then absolute totals that
	// replace the session's counts, and Subagents replaces the session's
	// subagent list. A source must set SessionID on a reset update even when
	// it carries no other data, so stale counts are cleared.
	Reset bool `json:"reset,omitempty"`

	MessageCountDelta    int    `json:"messageCountDelta,omitempty"`
	ToolCallCountDelta   int    `json:"toolCallCountDelta,omitempty"`
	CompactionCountDelta int    `json:"compactionCountDelta,omitempty"`
	CurrentTool          string `json:"currentTool,omitempty"`

	WorkingDir string `json:"workingDir,omitempty"`
	Branch     string `json:"branch,omitempty"`

	// SkippedLines is the number of lines this Parse call passed over
	// because they exceeded the source's per-line size cap. It is a delta,
	// not a total, and the monitor adds it to the source's health even when
	// SessionID is empty, so an update that carries only skipped lines
	// creates or refreshes no session.
	SkippedLines int `json:"skippedLines,omitempty"`

	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`

	Subagents []session.SubagentState `json:"subagents,omitempty"`

	Terminal  bool      `json:"terminal"`
	EndReason string    `json:"endReason,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}
