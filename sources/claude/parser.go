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

	// Fields present on type:"progress" records.
	ToolUseID       string          `json:"toolUseID,omitempty"`
	ParentToolUseID string          `json:"parentToolUseID,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
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
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`       // set on tool_use blocks
	ToolUseID string `json:"tool_use_id,omitempty"` // set on tool_result blocks
}

// tokenUsage holds per-turn token counts from the Claude API.
type tokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// progressDataHeader is used for a fast type check on progress data.
type progressDataHeader struct {
	Type string `json:"type"`
}

// progressData wraps the nested data.message structure inside a progress entry.
type progressData struct {
	Message struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	} `json:"message"`
}

// subagentResult accumulates parsed state for a single subagent.
// Keyed by toolUseID in parseResult.subagents.
type subagentResult struct {
	id              string
	parentToolUseID string
	slug            string
	model           string
	contextTokens   int
	outputTokens    int
	toolCalls       int
	currentTool     string
	activity        session.Activity
	firstTime       time.Time
	lastTime        time.Time
	completed       bool
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

	msgDelta        int
	toolDelta       int
	compactionDelta int
	currentTool     string

	activity       session.Activity
	startedAt      time.Time
	lastActivityAt time.Time

	subagents  map[string]*subagentResult // keyed by toolUseID
	parentMap  map[string]string          // parentToolUseID -> toolUseID (for cursor persistence)

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
//
// knownParents maps parentToolUseID → toolUseID for subagents tracked in
// prior batches. Pass nil when no prior state exists.
func parseLines(lines [][]byte) parseResult {
	return parseLinesWithParents(lines, nil)
}

func parseLinesWithParents(lines [][]byte, knownParents map[string]string) parseResult {
	r := parseResult{
		activity:  session.ActivityIdle,
		subagents: make(map[string]*subagentResult),
		parentMap: make(map[string]string),
	}

	// Seed parent map from prior batches.
	for pid, sid := range knownParents {
		r.parentMap[pid] = sid
	}

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
		// Only capture slug from non-progress entries (progress entries carry
		// subagent slugs, not the parent session's slug).
		if rec.Slug != "" && rec.Type != "progress" && r.slug == "" {
			r.slug = rec.Slug
		}

		switch rec.Type {
		case "user":
			if rec.Message != nil && rec.Message.Role == "user" {
				r.msgDelta++
				r.activity = session.ActivityWorking
				r.hasData = true
				checkSubagentCompletion(rec.Message.Content, &r)
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

		case "progress":
			parseProgressRecord(&rec, line, &r)

		case "compaction":
			r.compactionDelta++
		}
	}

	return r
}

// parseProgressRecord handles a type:"progress" JSONL line, accumulating
// subagent state into r.subagents keyed by toolUseID.
func parseProgressRecord(rec *jsonlRecord, line []byte, r *parseResult) {
	if rec.ToolUseID == "" {
		return
	}

	// Determine if this is an agent_progress entry (subagent vs. plain tool progress).
	isAgent := false
	if rec.Data != nil {
		var header progressDataHeader
		if json.Unmarshal(rec.Data, &header) == nil {
			isAgent = header.Type == "agent_progress"
		}
	}

	// Self-progress filter: skip non-agent entries whose slug matches the
	// session slug. Agent entries are never self-progress.
	if !isAgent {
		if rec.Slug != "" && r.slug != "" && rec.Slug == r.slug {
			return
		}
	}

	sub, exists := r.subagents[rec.ToolUseID]
	if !exists {
		// For agent entries, always create a subagent even without a slug.
		// For non-agent entries, require a slug to avoid creating phantoms.
		if !isAgent && rec.Slug == "" {
			return
		}
		sub = &subagentResult{
			id:              rec.ToolUseID,
			parentToolUseID: rec.ParentToolUseID,
			slug:            rec.Slug,
			activity:        session.ActivityWorking,
		}
		r.subagents[rec.ToolUseID] = sub
		if rec.ParentToolUseID != "" {
			r.parentMap[rec.ParentToolUseID] = rec.ToolUseID
		}
	}

	// Update slug if a later progress record provides one.
	if rec.Slug != "" && sub.slug == "" {
		sub.slug = rec.Slug
	}

	if rec.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
			if sub.firstTime.IsZero() {
				sub.firstTime = t
			}
			sub.lastTime = t
		}
	}

	if rec.Data == nil {
		return
	}
	var pd progressData
	if err := json.Unmarshal(rec.Data, &pd); err != nil {
		return
	}

	switch pd.Message.Type {
	case "assistant":
		if pd.Message.Message != nil {
			parseSubagentAssistantMessage(pd.Message.Message, sub)
		}
		if sub.activity != session.ActivityWorking {
			sub.activity = session.ActivityWaiting
		}
	case "user":
		sub.activity = session.ActivityWaiting
	}
}

// parseSubagentAssistantMessage extracts model, usage, and tool calls from
// a subagent's assistant message (the inner data.message.message object).
func parseSubagentAssistantMessage(raw json.RawMessage, sub *subagentResult) {
	var msg struct {
		Model   string         `json:"model"`
		Usage   *tokenUsage    `json:"usage,omitempty"`
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	if msg.Model != "" {
		sub.model = msg.Model
	}
	if msg.Usage != nil {
		u := msg.Usage
		sub.contextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		sub.outputTokens = u.OutputTokens
	}

	for i := 0; i < len(msg.Content); i++ {
		if msg.Content[i].Type == "tool_use" {
			sub.toolCalls++
			sub.currentTool = msg.Content[i].Name
			sub.activity = session.ActivityWorking
		}
	}
}

// checkSubagentCompletion scans a user message's content blocks for
// tool_result entries whose tool_use_id matches a known subagent's
// parentToolUseID (or a cross-batch entry from r.parentMap), marking
// that subagent as completed.
func checkSubagentCompletion(blocks []contentBlock, r *parseResult) {
	// Build lookup: parentToolUseID -> subagent toolUseID
	parentToSub := make(map[string]string, len(r.subagents)+len(r.parentMap))
	for id, sub := range r.subagents {
		if sub.parentToolUseID != "" {
			parentToSub[sub.parentToolUseID] = id
		}
	}
	// Merge known parents from prior batches (current batch takes precedence).
	for parentID, subID := range r.parentMap {
		if _, exists := parentToSub[parentID]; !exists {
			parentToSub[parentID] = subID
		}
	}
	if len(parentToSub) == 0 {
		return
	}

	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		if block.Type != "tool_result" || block.ToolUseID == "" {
			continue
		}
		subID, ok := parentToSub[block.ToolUseID]
		if !ok {
			continue
		}
		if sub, exists := r.subagents[subID]; exists {
			sub.completed = true
			sub.activity = session.ActivityTerminal
		} else {
			// Cross-batch: create a minimal entry to signal completion.
			r.subagents[subID] = &subagentResult{
				id:              subID,
				parentToolUseID: block.ToolUseID,
				completed:       true,
				activity:        session.ActivityTerminal,
			}
		}
	}
}

// buildSubagentStates converts internal subagent results to the public
// session.SubagentState slice for inclusion in a SourceUpdate.
func buildSubagentStates(subs map[string]*subagentResult) []session.SubagentState {
	if len(subs) == 0 {
		return nil
	}
	out := make([]session.SubagentState, 0, len(subs))
	for _, sub := range subs {
		out = append(out, session.SubagentState{
			ID:             sub.id,
			ParentID:       sub.parentToolUseID,
			Slug:           sub.slug,
			Activity:       sub.activity,
			CurrentTool:    sub.currentTool,
			StartedAt:      sub.firstTime,
			LastActivityAt: sub.lastTime,
		})
	}
	return out
}

// buildParentMap extracts the parentToolUseID -> toolUseID mapping from
// subagent results, for persistence in the next cursor.
func buildParentMap(subs map[string]*subagentResult) map[string]string {
	if len(subs) == 0 {
		return nil
	}
	m := make(map[string]string, len(subs))
	for id, sub := range subs {
		if sub.parentToolUseID != "" {
			m[sub.parentToolUseID] = id
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
