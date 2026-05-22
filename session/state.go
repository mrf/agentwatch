// Package session contains the public session state model for agentwatch.
package session

import (
	"encoding/json"
	"fmt"
	"time"
)

// Activity represents what an agent session is currently doing.
type Activity string

const (
	// ActivityIdle indicates the session is running but not actively processing.
	ActivityIdle Activity = "idle"
	// ActivityWorking indicates the session is actively processing a request.
	ActivityWorking Activity = "working"
	// ActivityWaiting indicates the session is waiting for user input.
	ActivityWaiting Activity = "waiting"
	// ActivityTerminal indicates the session has ended.
	ActivityTerminal Activity = "terminal"
)

// UnmarshalJSON implements json.Unmarshaler. It accepts any string value to
// remain forward-compatible, but unknown values round-trip as-is.
func (a *Activity) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	*a = Activity(s)
	return nil
}

// SubagentState tracks a nested agent spawned within a parent session.
type SubagentState struct {
	ID             string    `json:"id"`
	ParentID       string    `json:"parentId,omitempty"`
	Slug           string    `json:"slug,omitempty"`
	Activity       Activity  `json:"activity"`
	CurrentTool    string    `json:"currentTool,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

// SessionState is the public snapshot of a monitored agent session.
type SessionState struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Slug   string `json:"slug,omitempty"`

	Activity  Activity       `json:"activity"`
	Lifecycle LifecycleState `json:"lifecycle"`

	ContextTokens      int     `json:"contextTokens"`
	OutputTokens       int     `json:"outputTokens,omitempty"`
	TokenEstimated     bool    `json:"tokenEstimated"`
	MaxContextTokens   int     `json:"maxContextTokens"`
	ContextUtilization float64 `json:"contextUtilization"`

	Model       string `json:"model"`
	WorkingDir  string `json:"workingDir"`
	Branch      string `json:"branch,omitempty"`
	CurrentTool string `json:"currentTool,omitempty"`

	MessageCount    int `json:"messageCount"`
	ToolCallCount   int `json:"toolCallCount"`
	CompactionCount int `json:"compactionCount"`

	StartedAt          time.Time  `json:"startedAt"`
	LastActivityAt     time.Time  `json:"lastActivityAt"`
	LastDataReceivedAt time.Time  `json:"lastDataReceivedAt"`
	CompletedAt        *time.Time `json:"completedAt,omitempty"`

	Subagents []SubagentState `json:"subagents,omitempty"`
}

// ComputeContextUtilization returns ContextTokens/MaxContextTokens as a
// fraction in [0,1], or 0 if MaxContextTokens is zero.
func ComputeContextUtilization(contextTokens, maxContextTokens int) float64 {
	if maxContextTokens <= 0 {
		return 0
	}
	return float64(contextTokens) / float64(maxContextTokens)
}

// Clone returns a deep copy of s. Callers may mutate the copy without
// affecting the original.
func (s SessionState) Clone() SessionState {
	c := s
	if s.CompletedAt != nil {
		t := *s.CompletedAt
		c.CompletedAt = &t
	}
	if len(s.Subagents) > 0 {
		c.Subagents = make([]SubagentState, len(s.Subagents))
		copy(c.Subagents, s.Subagents)
	}
	return c
}
