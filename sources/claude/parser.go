package claude

import (
	"encoding/json"
	"time"

	"github.com/mrf/agentwatch/session"
)

// jsonlRecord is the top-level shape of a Claude Code JSONL record.
// Fields not relevant to monitoring are omitted.
type jsonlRecord struct {
	Type      string         `json:"type"`
	SessionID string         `json:"sessionId"`
	Timestamp string         `json:"timestamp"`
	CWD       string         `json:"cwd"`
	GitBranch string         `json:"gitBranch"`
	Slug      string         `json:"slug"`
	Message   *claudeMessage `json:"message,omitempty"`
}

// claudeMessage is the message field present in "user" and "assistant" records.
type claudeMessage struct {
	Role       string         `json:"role"`
	Model      string         `json:"model,omitempty"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason,omitempty"`
	Usage      *tokenUsage    `json:"usage,omitempty"`
}

// contentBlock is one element of message.content.
type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"` // set on tool_use blocks
}

// tokenUsage holds per-turn token counts from the Claude API.
type tokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// parseResult accumulates state across a batch of JSONL lines.
type parseResult struct {
	sessionID string
	slug      string
	model     string
	cwd       string
	branch    string

	contextTokens int
	outputTokens  int

	msgDelta    int
	toolDelta   int
	currentTool string

	activity       session.Activity
	startedAt      time.Time
	lastActivityAt time.Time

	// hasData is true when at least one user or enriched assistant record was parsed.
	hasData bool
}

// parseLines extracts monitoring state from a slice of raw JSONL lines.
//
// Claude Code records assistant turns as multiple streaming chunks sharing the
// same API message ID. Only chunks that contain the "cwd" field have been
// enriched by the Claude Code wrapper; earlier chunks are raw API events.
// This function counts enriched assistant records only to avoid over-counting
// turns.
func parseLines(lines [][]byte) parseResult {
	r := parseResult{activity: session.ActivityIdle}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(line) == 0 {
			continue
		}

		var rec jsonlRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // skip malformed lines
		}

		// Capture the earliest timestamp as startedAt, latest as lastActivityAt.
		if rec.Timestamp != "" {
			t, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err == nil {
				if r.startedAt.IsZero() {
					r.startedAt = t
				}
				r.lastActivityAt = t
			}
		}

		// Prefer the most recent non-empty values for session metadata.
		if rec.SessionID != "" {
			r.sessionID = rec.SessionID
		}
		if rec.CWD != "" {
			r.cwd = rec.CWD
		}
		if rec.GitBranch != "" {
			r.branch = rec.GitBranch
		}
		if rec.Slug != "" {
			r.slug = rec.Slug
		}

		switch rec.Type {
		case "user":
			if rec.Message != nil && rec.Message.Role == "user" {
				r.msgDelta++
				// A user message means the assistant is (or was) processing it.
				r.activity = session.ActivityWorking
				r.hasData = true
			}

		case "assistant":
			// Only count assistant records that carry the Claude Code wrapper
			// fields (cwd present). Records without cwd are raw API streaming
			// events that precede the final enriched record for the same turn.
			if rec.Message == nil || rec.Message.Role != "assistant" || rec.CWD == "" {
				continue
			}

			r.msgDelta++
			r.hasData = true

			if rec.Message.Model != "" {
				r.model = rec.Message.Model
			}

			if rec.Message.Usage != nil {
				u := rec.Message.Usage
				r.contextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				r.outputTokens = u.OutputTokens
			}

			// Count tool_use content blocks and record the last tool name.
			for j := 0; j < len(rec.Message.Content); j++ {
				if rec.Message.Content[j].Type == "tool_use" {
					r.toolDelta++
					r.currentTool = rec.Message.Content[j].Name
				}
			}

			switch rec.Message.StopReason {
			case "end_turn":
				r.activity = session.ActivityWaiting
			case "tool_use":
				r.activity = session.ActivityWorking
			}
		}
	}

	return r
}
