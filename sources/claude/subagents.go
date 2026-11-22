package claude

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/session"
)

// subagentsDirName is the directory Claude Code writes subagent transcripts
// into: <project>/<session-id>/subagents/agent-<agent-id>.jsonl. Records in
// those files carry the parent's sessionId, so the directory, not the record,
// is the parent link.
const subagentsDirName = "subagents"

// maxSubagentMetaSize caps the agent-<id>.meta.json sidecar read.
const maxSubagentMetaSize = 64 << 10

// subagentEntry is the per-subagent state carried in the cursor. State is
// cumulative across polls; Position is the read position in the subagent's
// own file and stays zero for progress-tracked (pre-subagents/ layout)
// subagents.
type subagentEntry struct {
	jsonl.Position
	LastMessageID string                `json:"lastMessageId,omitempty"` // last counted assistant message.id
	State         session.SubagentState `json:"state"`
}

// subagentMeta is the subset of agent-<id>.meta.json the source reads. The
// sidecar also holds the task description, which is deliberately not decoded.
type subagentMeta struct {
	ToolUseID string `json:"toolUseId"`
}

// isSubagentsDir reports whether path is a <session-id>/subagents directory
// below root, whose files belong to a parent session rather than being
// sessions of their own.
func isSubagentsDir(root, path string) bool {
	return filepath.Base(path) == subagentsDirName && filepath.Clean(path) != filepath.Clean(root)
}

// subagentFileID returns the agent ID for an agent-<id>.jsonl file name.
func subagentFileID(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, "agent-")
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, ".jsonl")
	return id, ok && id != "" && len(id) <= maxSessionIDLen && validSessionIDRe.MatchString(id)
}

// readSubagentFiles reads new lines from every subagent file belonging to the
// parent transcript at parentPath, folding them into cc.Subagents. It returns
// the IDs whose state changed (never nil), whether any file offset advanced,
// and how many oversized lines were skipped. Unreadable files are skipped and
// retried on the next poll.
func readSubagentFiles(parentPath string, cc *claudeCursor) (touched map[string]struct{}, advanced bool, skipped int) {
	touched = make(map[string]struct{})
	stem, ok := strings.CutSuffix(parentPath, ".jsonl")
	if !ok {
		return touched, false, 0
	}
	dir := filepath.Join(stem, subagentsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return touched, false, 0
	}

	for _, de := range entries {
		if !de.Type().IsRegular() {
			continue
		}
		id, ok := subagentFileID(de.Name())
		if !ok {
			continue
		}
		entry, changed, moved, n, ok := readSubagentFile(dir, de.Name(), id, cc.Subagents[id])
		if !ok {
			continue
		}
		if changed {
			touched[id] = struct{}{}
		}
		advanced = advanced || moved
		skipped += n
		cc.putSubagent(id, entry)
	}
	return touched, advanced, skipped
}

// readSubagentFile reads new lines of subagent id's file name in dir and folds
// them into entry. It reports whether the state changed, whether the offset
// advanced, the skipped-line count, and false if the file could not be read.
func readSubagentFile(dir, name, id string, entry subagentEntry) (out subagentEntry, changed, advanced bool, skipped int, ok bool) {
	res, next, reset, err := jsonl.ReadFrom(filepath.Join(dir, name), entry.Position, jsonl.Options{})
	if err != nil {
		return entry, false, false, 0, false
	}
	if reset {
		// The file is re-read from the start. Only the counts
		// accumulate; every other field is overwritten by the re-read.
		entry.State.MessageCount, entry.State.ToolCallCount = 0, 0
		entry.LastMessageID = ""
		changed = true
	}
	advanced = next != entry.Position
	entry.Position = next
	entry.State.ID = id
	if entry.State.ParentID == "" {
		entry.State.ParentID = readSubagentParentToolUseID(filepath.Join(dir, "agent-"+id+".meta.json"))
	}

	r := parseSubagentLines(res.Lines, entry.LastMessageID)
	entry.LastMessageID = r.lastMsgID
	if r.hasData || !r.lastActivityAt.IsZero() {
		entry.State = accumulateSubagent(entry.State, fileSubagentBatch(id, r))
		changed = true
	}
	return entry, changed, advanced, res.Skipped, true
}

func (cc *claudeCursor) putSubagent(id string, e subagentEntry) {
	if cc.Subagents == nil {
		cc.Subagents = make(map[string]subagentEntry)
	}
	cc.Subagents[id] = e
}

// readSubagentParentToolUseID returns the parent's Agent tool_use ID from the
// meta sidecar, or "" if it is absent or unreadable (retried next poll).
func readSubagentParentToolUseID(path string) string {
	data, ok := readBoundedRegularFile(path, maxSubagentMetaSize)
	if !ok {
		return ""
	}
	var meta subagentMeta
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.ToolUseID
}

// fileSubagentBatch converts one batch parsed from a subagent's own file into
// a batch state whose counts are deltas, for accumulateSubagent.
func fileSubagentBatch(id string, r parseResult) session.SubagentState {
	b := session.SubagentState{
		ID:             id,
		CurrentTool:    r.currentTool,
		StartedAt:      r.startedAt,
		LastActivityAt: r.lastActivityAt,
		Model:          r.model,
		ContextTokens:  r.contextTokens,
		OutputTokens:   r.outputTokens,
		MessageCount:   r.msgDelta,
		ToolCallCount:  r.toolDelta,
	}
	if r.hasData {
		b.Activity = r.activity
	}
	return b
}

// accumulateSubagent folds a batch into the cumulative state prev. Batch
// counts are deltas and are added; other non-zero batch fields overwrite,
// except that StartedAt keeps the earliest value and a terminal subagent
// stays terminal.
func accumulateSubagent(prev, b session.SubagentState) session.SubagentState {
	s := prev
	s.ID = b.ID
	if b.ParentID != "" {
		s.ParentID = b.ParentID
	}
	if b.Slug != "" {
		s.Slug = b.Slug
	}
	if b.Model != "" {
		s.Model = b.Model
	}
	if b.CurrentTool != "" {
		s.CurrentTool = b.CurrentTool
	}
	if b.ContextTokens != 0 || b.OutputTokens != 0 {
		s.ContextTokens = b.ContextTokens
		s.OutputTokens = b.OutputTokens
	}
	s.MessageCount += b.MessageCount
	s.ToolCallCount += b.ToolCallCount
	accumulateSubagentTimes(&s, b)
	if b.Activity != "" && s.Activity != session.ActivityTerminal {
		s.Activity = b.Activity
	}
	return s
}

// accumulateSubagentTimes keeps the earliest StartedAt and latest
// LastActivityAt, and takes a non-zero CompletedAt from b.
func accumulateSubagentTimes(s *session.SubagentState, b session.SubagentState) {
	if !b.StartedAt.IsZero() && (s.StartedAt.IsZero() || b.StartedAt.Before(s.StartedAt)) {
		s.StartedAt = b.StartedAt
	}
	if b.LastActivityAt.After(s.LastActivityAt) {
		s.LastActivityAt = b.LastActivityAt
	}
	if !b.CompletedAt.IsZero() {
		s.CompletedAt = b.CompletedAt
	}
}

// knownParents returns parentToolUseID -> subagent ID for every subagent the
// cursor knows about, so a parent tool_result can complete it.
func (cc *claudeCursor) knownParents() map[string]string {
	m := make(map[string]string, len(cc.Parents)+len(cc.Subagents))
	maps.Copy(m, cc.Parents)
	for id, e := range cc.Subagents {
		if e.State.ParentID != "" {
			m[e.State.ParentID] = id
		}
	}
	return m
}

// subagentLastMessageIDs returns each subagent's last counted message.id, so
// progress records continuing a split message across polls are not recounted.
func (cc *claudeCursor) subagentLastMessageIDs() map[string]string {
	m := make(map[string]string, len(cc.Subagents))
	for id, e := range cc.Subagents {
		if e.LastMessageID != "" {
			m[id] = e.LastMessageID
		}
	}
	return m
}

// pruneParents drops the links of completed subagents, which no later
// tool_result can complete again, so the cursor does not grow with every
// subagent the session has ever run.
func (cc *claudeCursor) pruneParents() {
	for parentID, id := range cc.Parents {
		if cc.Subagents[id].State.Activity == session.ActivityTerminal {
			delete(cc.Parents, parentID)
		}
	}
}

// subagentStates returns the cumulative states for ids, sorted by ID.
func (cc *claudeCursor) subagentStates(ids map[string]struct{}) []session.SubagentState {
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	out := make([]session.SubagentState, 0, len(sorted))
	for i := 0; i < len(sorted); i++ {
		out = append(out, cc.Subagents[sorted[i]].State)
	}
	return out
}
