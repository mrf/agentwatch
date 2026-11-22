package claude

import (
	"encoding/json"
	"maps"
	"time"

	"github.com/mrf/agentwatch/session"
)

const syntheticModel = "<synthetic>"

// JSONL record / content-block type names.
const (
	typeUser      = "user"
	typeAssistant = "assistant"
	typeToolUse   = "tool_use"
)

// jsonlRecord is the top-level shape of a Claude Code JSONL record.
// Fields not relevant to monitoring are omitted.
type jsonlRecord struct {
	Type      string         `json:"type"`
	SessionID string         `json:"sessionId"`
	Timestamp string         `json:"timestamp"`
	CWD       string         `json:"cwd"`
	Subtype   string         `json:"subtype"`
	GitBranch string         `json:"gitBranch"`
	Slug      string         `json:"slug"`
	Message   *claudeMessage `json:"message,omitempty"`

	// IsSidechain marks records of a subagent's own conversation.
	IsSidechain bool `json:"isSidechain,omitempty"`
	// IsCompactSummary marks the synthetic user record that follows a
	// compact_boundary and carries the conversation summary.
	IsCompactSummary bool `json:"isCompactSummary,omitempty"`

	// Fields present on type:"progress" records.
	ToolUseID       string          `json:"toolUseID,omitempty"`
	ParentToolUseID string          `json:"parentToolUseID,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// claudeMessage is the message field present in "user" and "assistant" records.
type claudeMessage struct {
	ID         string         `json:"id,omitempty"`
	Role       string         `json:"role"`
	Model      string         `json:"model,omitempty"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason,omitempty"`
	Usage      *tokenUsage    `json:"usage,omitempty"`
}

// contentBlock is one element of message.content.
type contentBlock struct {
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`        // set on tool_use blocks
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
	msgCount        int
	lastMsgID       string // message.id of the last counted progress message
	currentTool     string
	activity        session.Activity
	firstTime       time.Time
	lastTime        time.Time
	completed       bool
	completedAt     time.Time
}

// parseResult accumulates state across a batch of JSONL lines.
type parseResult struct {
	sessionID string
	slug      string
	model     string
	cwd       string
	branch    string

	// hasTokens is true when an assistant usage record set the token counts.
	hasTokens     bool
	contextTokens int
	outputTokens  int

	msgDelta        int
	lastMsgID       string // message.id of the last counted assistant record
	toolDelta       int
	compactionDelta int
	currentTool     string

	activity       session.Activity
	startedAt      time.Time
	lastActivityAt time.Time

	subagents map[string]*subagentResult // keyed by toolUseID
	parentMap map[string]string          // parentToolUseID -> toolUseID (for cursor persistence)

	// subLastMsgIDs seeds each progress-tracked subagent's message.id dedupe
	// from the previous batch, keyed by toolUseID.
	subLastMsgIDs map[string]string

	// hasData is true when at least one user or enriched assistant record was parsed.
	hasData bool
}

// parseLines extracts monitoring state from a slice of raw JSONL lines.
//
// Claude Code records assistant turns as multiple streaming chunks sharing the
// same API message ID. Only chunks that contain the "cwd" field have been
// enriched by the Claude Code wrapper; earlier chunks are raw API events.
// This function counts enriched assistant records only to avoid over-counting
// turns, and counts consecutive records sharing a message.id once: Claude Code
// writes one record per content block of a message.
//
// knownParents maps parentToolUseID → toolUseID for subagents tracked in
// prior batches. Pass nil when no prior state exists.
func parseLines(lines [][]byte) parseResult {
	return parseLinesWithParents(lines, nil)
}

func parseLinesWithParents(lines [][]byte, knownParents map[string]string) parseResult {
	return parseRecords(lines, knownParents, "", nil, false)
}

// parseSubagentLines parses a subagent's own transcript, where every record
// is isSidechain:true and is that subagent's state rather than the parent's.
// lastMsgID is the last message.id counted by the previous batch.
func parseSubagentLines(lines [][]byte, lastMsgID string) parseResult {
	return parseRecords(lines, nil, lastMsgID, nil, true)
}

// parseRecords is the shared parser. With includeSidechain false, isSidechain
// records are skipped entirely so they cannot overwrite parent session state.
// lastMsgID seeds the assistant message.id dedupe from the previous batch;
// subLastMsgIDs does the same for each progress-tracked subagent.
func parseRecords(lines [][]byte, knownParents map[string]string, lastMsgID string, subLastMsgIDs map[string]string, includeSidechain bool) parseResult {
	r := parseResult{
		lastMsgID:     lastMsgID,
		activity:      session.ActivityIdle,
		subagents:     make(map[string]*subagentResult),
		parentMap:     make(map[string]string),
		subLastMsgIDs: subLastMsgIDs,
	}

	// Seed parent map from prior batches.
	maps.Copy(r.parentMap, knownParents)

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(line) == 0 {
			continue
		}

		var rec jsonlRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // skip malformed lines
		}
		if rec.IsSidechain && !includeSidechain {
			continue
		}
		r.applyRecord(&rec)
	}

	return r
}

// applyRecord folds one decoded, in-scope record into r.
func (r *parseResult) applyRecord(rec *jsonlRecord) {
	// Capture the earliest timestamp as startedAt, latest as lastActivityAt.
	if t, ok := parseTimestamp(rec.Timestamp); ok {
		extendSpan(&r.startedAt, &r.lastActivityAt, t)
	}
	r.applyMetadata(rec)

	switch rec.Type {
	case typeUser:
		r.applyUser(rec)
	case typeAssistant:
		r.applyAssistant(rec)
	case "progress":
		parseProgressRecord(rec, r)
	case "system":
		if rec.Subtype == "compact_boundary" {
			r.compactionDelta++
		}
	}
}

// parseTimestamp parses an RFC 3339 record timestamp; empty or malformed
// values report false.
func parseTimestamp(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// extendSpan records t as the latest time, and as the first when none is set.
func extendSpan(first, last *time.Time, t time.Time) {
	if first.IsZero() {
		*first = t
	}
	*last = t
}

// applyMetadata keeps the most recent non-empty session metadata.
func (r *parseResult) applyMetadata(rec *jsonlRecord) {
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
}

func (r *parseResult) applyUser(rec *jsonlRecord) {
	if rec.IsCompactSummary {
		return
	}
	if rec.Message != nil && rec.Message.Role == typeUser {
		r.msgDelta++
		r.activity = session.ActivityWorking
		r.hasData = true
		checkSubagentCompletion(rec.Message.Content, r, r.lastActivityAt)
	}
}

func (r *parseResult) applyAssistant(rec *jsonlRecord) {
	// Only count assistant records that carry the Claude Code wrapper
	// fields (cwd present). Records without cwd are raw API streaming
	// events that precede the final enriched record for the same turn.
	if rec.Message == nil || rec.Message.Role != typeAssistant || rec.CWD == "" {
		return
	}

	r.countAssistantMessage(rec.Message.ID)
	r.hasData = true
	r.applyModelUsage(rec.Message)

	// Count tool_use content blocks and record the last tool name.
	for j := 0; j < len(rec.Message.Content); j++ {
		if rec.Message.Content[j].Type == typeToolUse {
			r.toolDelta++
			r.currentTool = rec.Message.Content[j].Name
		}
	}

	switch rec.Message.StopReason {
	case "end_turn":
		r.activity = session.ActivityWaiting
	case typeToolUse:
		r.activity = session.ActivityWorking
	}
}

// countAssistantMessage counts a record once per consecutive message.id.
func (r *parseResult) countAssistantMessage(id string) {
	if countsMessage(id, &r.lastMsgID) {
		r.msgDelta++
	}
}

// countsMessage reports whether a record with message id starts a new
// message, advancing *last when it does. Records without an id cannot be
// deduplicated and always count.
func countsMessage(id string, last *string) bool {
	if id == "" {
		return true
	}
	if id == *last {
		return false
	}
	*last = id
	return true
}

// applyModelUsage records an assistant message's model and token usage.
// Locally generated turns (API errors, interrupts) carry the placeholder
// model "<synthetic>" and zeroed usage; neither describes the real
// conversation, so both are ignored.
func (r *parseResult) applyModelUsage(msg *claudeMessage) {
	if msg.Model == syntheticModel {
		return
	}
	if msg.Model != "" {
		r.model = msg.Model
	}
	if msg.Usage != nil {
		u := msg.Usage
		r.hasTokens = true
		r.contextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		r.outputTokens = u.OutputTokens
	}
}

// parseProgressRecord handles a type:"progress" JSONL line, accumulating
// subagent state into r.subagents keyed by toolUseID.
func parseProgressRecord(rec *jsonlRecord, r *parseResult) {
	if rec.ToolUseID == "" {
		return
	}

	sub := r.progressSubagent(rec, isAgentProgress(rec))
	if sub == nil {
		return
	}

	// Update slug if a later progress record provides one.
	if rec.Slug != "" && sub.slug == "" {
		sub.slug = rec.Slug
	}

	if t, ok := parseTimestamp(rec.Timestamp); ok {
		extendSpan(&sub.firstTime, &sub.lastTime, t)
	}

	if rec.Data == nil {
		return
	}
	var pd progressData
	if err := json.Unmarshal(rec.Data, &pd); err != nil {
		return
	}
	applyProgressMessage(&pd, sub)
}

// isAgentProgress reports whether rec is an agent_progress entry (subagent)
// rather than plain tool progress.
func isAgentProgress(rec *jsonlRecord) bool {
	if rec.Data == nil {
		return false
	}
	var header progressDataHeader
	if json.Unmarshal(rec.Data, &header) != nil {
		return false
	}
	return header.Type == "agent_progress"
}

// progressSubagent returns the subagent a progress record describes,
// creating it on first sight, or nil when the record must be ignored.
func (r *parseResult) progressSubagent(rec *jsonlRecord, isAgent bool) *subagentResult {
	// Self-progress filter: skip non-agent entries whose slug matches the
	// session slug. Agent entries are never self-progress.
	if !isAgent && rec.Slug != "" && r.slug != "" && rec.Slug == r.slug {
		return nil
	}

	if sub, exists := r.subagents[rec.ToolUseID]; exists {
		return sub
	}
	// For agent entries, always create a subagent even without a slug.
	// For non-agent entries, require a slug to avoid creating phantoms.
	if !isAgent && rec.Slug == "" {
		return nil
	}
	sub := &subagentResult{
		id:              rec.ToolUseID,
		parentToolUseID: rec.ParentToolUseID,
		slug:            rec.Slug,
		activity:        session.ActivityWorking,
	}
	r.addSubagent(sub)
	if rec.ParentToolUseID != "" {
		r.parentMap[rec.ParentToolUseID] = rec.ToolUseID
	}
	return sub
}

// addSubagent registers sub, resuming its message.id dedupe from the cursor.
func (r *parseResult) addSubagent(sub *subagentResult) {
	sub.lastMsgID = r.subLastMsgIDs[sub.id]
	r.subagents[sub.id] = sub
}

// applyProgressMessage counts the inner message of a progress entry into sub.
func applyProgressMessage(pd *progressData, sub *subagentResult) {
	switch pd.Message.Type {
	case typeAssistant:
		var id string
		if pd.Message.Message != nil {
			id = parseSubagentAssistantMessage(pd.Message.Message, sub)
		}
		if countsMessage(id, &sub.lastMsgID) {
			sub.msgCount++
		}
		if sub.activity != session.ActivityWorking {
			sub.activity = session.ActivityWaiting
		}
	case typeUser:
		sub.msgCount++
		sub.activity = session.ActivityWaiting
	}
}

// parseSubagentAssistantMessage extracts model, usage, and tool calls from
// a subagent's assistant message (the inner data.message.message object),
// returning its message.id ("" when absent or undecodable).
func parseSubagentAssistantMessage(raw json.RawMessage, sub *subagentResult) string {
	var msg struct {
		ID      string         `json:"id"`
		Model   string         `json:"model"`
		Usage   *tokenUsage    `json:"usage,omitempty"`
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
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
		if msg.Content[i].Type == typeToolUse {
			sub.toolCalls++
			sub.currentTool = msg.Content[i].Name
			sub.activity = session.ActivityWorking
		}
	}
	return msg.ID
}

// checkSubagentCompletion scans a user message's content blocks for
// tool_result entries whose tool_use_id matches a known subagent's
// parentToolUseID (or a cross-batch entry from r.parentMap), marking
// that subagent as completed. at is the timestamp of the user message.
func checkSubagentCompletion(blocks []contentBlock, r *parseResult, at time.Time) {
	parentToSub := r.parentToSubagent()
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
			sub.completedAt = at
		} else {
			// Cross-batch: create a minimal entry to signal completion.
			r.addSubagent(&subagentResult{
				id:              subID,
				parentToolUseID: block.ToolUseID,
				completed:       true,
				activity:        session.ActivityTerminal,
				completedAt:     at,
			})
		}
	}
}

// parentToSubagent maps parentToolUseID to subagent toolUseID across this
// batch's subagents and the known parents from prior batches (current batch
// takes precedence).
func (r *parseResult) parentToSubagent() map[string]string {
	parentToSub := make(map[string]string, len(r.subagents)+len(r.parentMap))
	for id, sub := range r.subagents {
		if sub.parentToolUseID != "" {
			parentToSub[sub.parentToolUseID] = id
		}
	}
	for parentID, subID := range r.parentMap {
		if _, exists := parentToSub[parentID]; !exists {
			parentToSub[parentID] = subID
		}
	}
	return parentToSub
}

// buildSubagentStates converts internal subagent results to the public
// session.SubagentState slice for inclusion in an Update.
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
			Model:          sub.model,
			ContextTokens:  sub.contextTokens,
			OutputTokens:   sub.outputTokens,
			MessageCount:   sub.msgCount,
			ToolCallCount:  sub.toolCalls,
			CompletedAt:    sub.completedAt,
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
