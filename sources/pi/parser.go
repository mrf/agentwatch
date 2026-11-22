package pi

import (
	"encoding/json"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// bashToolName is the tool name reported for bashExecution entries.
const bashToolName = "Bash"

// piEntry is the top-level shape of every Pi JSONL record.
// All entries share id, parentId, and timestamp; type-specific fields vary.
type piEntry struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ParentID  string `json:"parentId"`
	Timestamp string `json:"timestamp"`

	// session header fields
	Version    int    `json:"version,omitempty"`
	WorkingDir string `json:"workingDir,omitempty"`

	// message wraps one of the AgentMessage variants
	Message json.RawMessage `json:"message,omitempty"`

	// model_change fields
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
}

// agentMessage is the shape of the "message" field inside a "message" entry.
// Fields from all AgentMessage variants are merged here; unused fields are zero.
type agentMessage struct {
	Role string `json:"role"`

	// AssistantMessage fields
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`

	// ToolResultMessage fields
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`

	// BashExecutionMessage fields
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
}

// tokenUsage holds per-turn token counts from Pi sessions. Pi's self-reported
// "cost" (a number in older files, an object in current ones) is deliberately
// not decoded.
type tokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
}

// accumulator collects incremental fields from JSONL lines.
type accumulator struct {
	sessionID      string
	model          string
	workingDir     string
	activity       session.Activity
	currentTool    string
	hasTokens      bool
	contextTokens  int
	outputTokens   int
	messageDelta   int
	toolCallDelta  int
	startedAt      time.Time
	lastActivityAt time.Time
}

func (a *accumulator) toUpdate() source.Update {
	return source.Update{
		SessionID:          a.sessionID,
		Model:              a.model,
		WorkingDir:         a.workingDir,
		Activity:           a.activity,
		CurrentTool:        a.currentTool,
		HasTokens:          a.hasTokens,
		ContextTokens:      a.contextTokens,
		OutputTokens:       a.outputTokens,
		MessageCountDelta:  a.messageDelta,
		ToolCallCountDelta: a.toolCallDelta,
		StartedAt:          a.startedAt,
		LastActivityAt:     a.lastActivityAt,
	}
}

// parseLine dispatches a single JSONL line into the accumulator.
// firstLine is true only for the very first line of the file (offset zero).
func parseLine(line []byte, firstLine bool, a *accumulator) {
	var e piEntry
	if json.Unmarshal(line, &e) != nil {
		return // skip malformed lines
	}

	a.noteTimestamp(e.Timestamp)

	switch e.Type {
	case "session":
		a.parseSessionHeader(&e)

	case "message":
		if len(e.Message) == 0 {
			return
		}
		parseMessage(e.Message, a)

	case "model_change":
		if e.ModelID != "" {
			a.model = e.ModelID
		}
	}

	_ = firstLine // used via startedAt tracking above
}

// noteTimestamp tracks timestamps: first seen becomes startedAt, latest
// becomes lastActivityAt.
func (a *accumulator) noteTimestamp(ts string) {
	if ts == "" {
		return
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return
	}
	if a.startedAt.IsZero() {
		a.startedAt = t
	}
	a.lastActivityAt = t
}

// parseSessionHeader captures the session ID and working dir from the session
// header, which is always the first line.
func (a *accumulator) parseSessionHeader(e *piEntry) {
	if e.ID != "" {
		a.sessionID = e.ID
	}
	if e.WorkingDir != "" {
		a.workingDir = e.WorkingDir
	}
}

func parseMessage(raw json.RawMessage, a *accumulator) {
	var msg agentMessage
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	switch msg.Role {
	case "user":
		a.messageDelta++
		a.activity = session.ActivityWorking

	case "assistant":
		a.parseAssistant(&msg)

	case "toolResult":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		if msg.ToolName != "" {
			a.currentTool = msg.ToolName
		}

	case "bashExecution":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = bashToolName
	}
}

func (a *accumulator) parseAssistant(msg *agentMessage) {
	a.messageDelta++
	// toolUse (or no stop reason yet) means the turn continues; any other
	// stop reason ends it.
	if msg.StopReason == "" || msg.StopReason == "toolUse" {
		a.activity = session.ActivityWorking
	} else {
		a.activity = session.ActivityWaiting
	}
	if msg.Model != "" {
		a.model = msg.Model
	}
	// Usage is decoded separately so an unreadable usage object costs
	// only the token counts, not the whole message.
	var usage tokenUsage
	if len(msg.Usage) > 0 && json.Unmarshal(msg.Usage, &usage) == nil {
		a.hasTokens = true
		a.contextTokens = usage.Input + usage.CacheRead + usage.CacheWrite
		a.outputTokens = usage.Output
	}
}
