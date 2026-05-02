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
	// any error. If no new data is available, it returns a zero SourceUpdate
	// and the same cursor.
	Parse(ctx context.Context, h SessionHandle, c Cursor) (SourceUpdate, Cursor, error)
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

// SourceUpdate carries incremental session data returned by Parse.
// Delta fields (MessageCountDelta, ToolCallCountDelta) represent the change
// since the previous Parse call; the monitor accumulates them.
type SourceUpdate struct {
	SessionID string `json:"sessionId"`
	Slug      string `json:"slug,omitempty"`

	Activity session.Activity `json:"activity"`
	Model    string           `json:"model,omitempty"`

	ContextTokens    int  `json:"contextTokens"`
	OutputTokens     int  `json:"outputTokens,omitempty"`
	MaxContextTokens int  `json:"maxContextTokens"`
	TokenEstimated   bool `json:"tokenEstimated"`

	MessageCountDelta  int    `json:"messageCountDelta,omitempty"`
	ToolCallCountDelta int    `json:"toolCallCountDelta,omitempty"`
	CurrentTool        string `json:"currentTool,omitempty"`

	WorkingDir string `json:"workingDir,omitempty"`
	Branch     string `json:"branch,omitempty"`

	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`

	Subagents []session.SubagentState `json:"subagents,omitempty"`

	Terminal  bool      `json:"terminal"`
	EndReason string    `json:"endReason,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}
