// Package codex provides the OpenAI Codex CLI source for agentwatch.
//
// # CODEX_HOME resolution
//
// This source does NOT read the CODEX_HOME environment variable. Callers must
// supply the base directory explicitly via WithRoot. If no WithRoot is
// provided the source returns no sessions. A caller that wants environment
// fallback can implement it itself:
//
//	root := os.Getenv("CODEX_HOME")
//	if root == "" {
//	    home, _ := os.UserHomeDir()
//	    root = filepath.Join(home, ".codex")
//	}
//	src := codex.New(codex.WithRoot(root))
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Source implements source.Source for OpenAI Codex CLI sessions.
//
// Codex CLI stores sessions at:
//
//	<root>/sessions/YYYY/MM/DD/rollout-{timestamp}-{uuid}.jsonl
type Source struct {
	root   string
	window time.Duration
	walker *filewatch.Walker
}

// Option configures a Source.
type Option func(*Source)

// WithRoot sets the Codex home directory (the directory containing
// "sessions/"). Without this option the source discovers no sessions.
func WithRoot(path string) Option {
	return func(s *Source) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to rollout files whose modification
// time is within d of the current time. Zero disables age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *Source) {
		s.window = d
	}
}

// New creates a new Codex source with the given options.
func New(opts ...Option) *Source {
	s := &Source{}
	for _, o := range opts {
		o(s)
	}
	s.buildWalker()
	return s
}

func (s *Source) buildWalker() {
	if s.root == "" {
		return
	}
	sessionsDir := filepath.Join(s.root, "sessions")
	wopts := []filewatch.Option{
		filewatch.WithPattern("rollout-*.jsonl"),
	}
	if s.window > 0 {
		wopts = append(wopts, filewatch.WithMaxAge(s.window))
	}
	s.walker = filewatch.New([]string{sessionsDir}, wopts...)
}

// Register registers this source with a source.Registry using a factory that
// calls New(opts...).
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("codex", func() (source.Source, error) {
		return New(opts...), nil
	})
}

// Name implements source.Source.
func (s *Source) Name() string { return "codex" }

// Discover implements source.Source. It walks <root>/sessions for recently
// modified rollout JSONL files and returns a handle for each.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	handles := make([]source.SessionHandle, 0, len(entries))
	for _, e := range entries {
		name := filepath.Base(e.Path)
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sessionID := sessionIDFromFilename(name)
		handles = append(handles, source.SessionHandle{
			ID:        sessionID,
			Path:      e.Path,
			StartedAt: e.ModTime, // refined by parser on first Parse call
			Source:    "codex",
		})
	}
	return handles, nil
}

// Parse implements source.Source. The cursor is the byte offset into the
// rollout file as a decimal string. An empty cursor means start from zero.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	var offset int64
	if c != "" {
		var err error
		offset, err = strconv.ParseInt(string(c), 10, 64)
		if err != nil {
			return source.SourceUpdate{}, c, fmt.Errorf("codex: invalid cursor %q: %w", c, err)
		}
	}

	lines, nextOffset, err := jsonl.ReadLines(h.Path, offset, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, fmt.Errorf("codex: read %s: %w", h.Path, err)
	}

	isFirst := offset == 0
	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], isFirst && i == 0, &acc)
	}

	update := acc.toUpdate()
	next := source.Cursor(strconv.FormatInt(nextOffset, 10))
	return update, next, nil
}

// ---- filename parsing -------------------------------------------------------

// sessionIDFromFilename extracts the session UUID from a rollout filename.
//
// New format: rollout-{ISO-timestamp}-{uuid}.jsonl
//
//	e.g. rollout-2026-04-24T13-10-05-019dc11d-218b-7ec1-a103-aeb16330d302.jsonl
//
// Old format: rollout-{unix-epoch}-{uuid}.jsonl
//
//	e.g. rollout-1738000000-0199e96c-7d0c-7403-bf30-395693cd1788.jsonl
//
// The UUID is the last 36 characters of the stem (8-4-4-4-12).
func sessionIDFromFilename(name string) string {
	stem := strings.TrimSuffix(name, ".jsonl")
	stem = strings.TrimPrefix(stem, "rollout-")

	if len(stem) >= 36 {
		candidate := stem[len(stem)-36:]
		if isUUID(candidate) {
			return candidate
		}
	}

	// Fallback: reconstruct from last five dash-separated segments.
	parts := strings.Split(stem, "-")
	if len(parts) >= 5 {
		candidate := strings.Join(parts[len(parts)-5:], "-")
		if len(candidate) == 36 {
			return candidate
		}
	}

	return stem
}

func isUUID(s string) bool {
	return len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

// ---- line parsing -----------------------------------------------------------

// accumulator collects incremental fields from JSONL lines.
type accumulator struct {
	sessionID        string
	model            string
	workingDir       string
	activity         session.Activity
	currentTool      string
	contextTokens    int
	outputTokens     int
	maxContextTokens int
	messageDelta     int
	toolCallDelta    int
	startedAt        time.Time
	lastActivityAt   time.Time
}

func (a *accumulator) toUpdate() source.SourceUpdate {
	return source.SourceUpdate{
		SessionID:          a.sessionID,
		Model:              a.model,
		WorkingDir:         a.workingDir,
		Activity:           a.activity,
		CurrentTool:        a.currentTool,
		ContextTokens:      a.contextTokens,
		OutputTokens:       a.outputTokens,
		MaxContextTokens:   a.maxContextTokens,
		MessageCountDelta:  a.messageDelta,
		ToolCallCountDelta: a.toolCallDelta,
		StartedAt:          a.startedAt,
		LastActivityAt:     a.lastActivityAt,
	}
}

// parseLine dispatches a single JSONL line into the accumulator.
// firstLine is true only for the very first line of the file (offset zero).
func parseLine(line []byte, firstLine bool, a *accumulator) {
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return
	}

	if envelope.Type != "" && len(envelope.Payload) > 0 {
		parseEnvelope(envelope.Type, envelope.Payload, a)
		return
	}

	// Old bare format.
	if firstLine {
		parseSessionMeta(line, a)
	} else {
		parseBareItem(line, a)
	}
}

func parseEnvelope(typ string, payload json.RawMessage, a *accumulator) {
	switch typ {
	case "session_meta":
		parseSessionMeta(payload, a)

	case "event_msg":
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
			// top-level info for token_count (newer Codex CLI format)
			Info json.RawMessage `json:"info"`
			// top-level model_context_window for task_started
			ModelContextWindow int `json:"model_context_window"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return
		}
		switch event.Type {
		case "user_message":
			a.messageDelta++
			a.activity = session.ActivityWaiting
		case "agent_message":
			a.messageDelta++
			a.activity = session.ActivityWorking
		case "agent_reasoning":
			a.activity = session.ActivityWorking
		case "token_count":
			// Newer format: top-level "info" inside the event_msg payload.
			if len(event.Info) > 0 && string(event.Info) != "null" {
				parseTokenCountInfo(event.Info, a)
			} else if len(event.Payload) > 0 {
				parseTokenCountFlat(event.Payload, a)
			}
		case "task_started":
			// model_context_window appears directly in the event_msg payload.
			if event.ModelContextWindow > 0 {
				a.maxContextTokens = event.ModelContextWindow
			}
		case "turn_started":
			parseTurnContext(event.Payload, a)
		case "tool_call":
			parseToolCall(event.Payload, a)
		case "session_configured":
			var cfg struct {
				Model json.RawMessage `json:"model"`
			}
			if json.Unmarshal(event.Payload, &cfg) == nil {
				if m := parseModel(cfg.Model); m != "" {
					a.model = m
				}
			}
		}

	case "response_item":
		parseResponseItem(payload, a)

	case "env_context":
		var env struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(payload, &env) == nil && env.Cwd != "" {
			a.workingDir = env.Cwd
		}

	case "turn_context":
		parseTurnContext(payload, a)
	}
}

// parseSessionMeta extracts session ID, model, and start time from a
// session_meta payload. Used by both the envelope and bare formats.
func parseSessionMeta(data json.RawMessage, a *accumulator) {
	var meta struct {
		ID             string          `json:"id"`
		SessionID      string          `json:"session_id"`
		ConversationID string          `json:"conversation_id"`
		Model          json.RawMessage `json:"model"`
		Timestamp      string          `json:"timestamp"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return
	}
	if id := firstNonEmpty(meta.ID, meta.SessionID, meta.ConversationID); id != "" {
		a.sessionID = id
	}
	if m := parseModel(meta.Model); m != "" {
		a.model = m
	}
	if meta.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
			a.startedAt = t
		}
	}
}

func parseBareItem(line []byte, a *accumulator) {
	var item struct {
		Type     string `json:"type"`
		ToolName string `json:"tool_name"`
		Name     string `json:"name"`
		Command  string `json:"command"`
	}
	if json.Unmarshal(line, &item) != nil {
		return
	}
	switch item.Type {
	case "message":
		a.messageDelta++
		a.activity = session.ActivityWorking
	case "reasoning":
		a.activity = session.ActivityWorking
	case "command_execution":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "Bash"
	case "file_change":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "FileEdit"
	case "web_search":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "WebSearch"
	case "mcp_tool_call":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		if t := firstNonEmpty(item.ToolName, item.Name); t != "" {
			a.currentTool = t
		}
	case "tool_call":
		parseToolCall(line, a)
	case "token_count":
		parseTokenCountFlat(line, a)
	}
}

func parseResponseItem(payload json.RawMessage, a *accumulator) {
	var item struct {
		Type     string `json:"type"`
		ToolName string `json:"tool_name"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(payload, &item) != nil {
		return
	}
	switch item.Type {
	case "message":
		a.messageDelta++
		a.activity = session.ActivityWorking
	case "reasoning":
		a.activity = session.ActivityWorking
	case "command_execution":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "Bash"
	case "file_change":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "FileEdit"
	case "web_search":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		a.currentTool = "WebSearch"
	case "mcp_tool_call":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		if t := firstNonEmpty(item.ToolName, item.Name); t != "" {
			a.currentTool = t
		}
	case "function_call", "custom_tool_call":
		a.toolCallDelta++
		a.activity = session.ActivityWorking
		if item.Name != "" {
			a.currentTool = item.Name
		}
	case "tool_call":
		parseToolCall(payload, a)
	}
}

func parseToolCall(payload json.RawMessage, a *accumulator) {
	var tool struct {
		Name     string `json:"name"`
		ToolName string `json:"tool_name"`
		Tool     struct {
			Name string `json:"name"`
		} `json:"tool"`
	}
	if json.Unmarshal(payload, &tool) != nil {
		return
	}
	a.toolCallDelta++
	a.activity = session.ActivityWorking
	if t := firstNonEmpty(tool.ToolName, tool.Name, tool.Tool.Name); t != "" {
		a.currentTool = t
	}
}

func parseTurnContext(payload json.RawMessage, a *accumulator) {
	var tc struct {
		Cwd                string          `json:"cwd"`
		Model              json.RawMessage `json:"model"`
		ModelContextWindow int             `json:"model_context_window"`
	}
	if json.Unmarshal(payload, &tc) != nil {
		return
	}
	if tc.Cwd != "" {
		a.workingDir = tc.Cwd
	}
	if m := parseModel(tc.Model); m != "" {
		a.model = m
	}
	if tc.ModelContextWindow > 0 {
		a.maxContextTokens = tc.ModelContextWindow
	}
}

// parseTokenCountInfo handles the nested "info" block from newer Codex CLI.
// It prefers last_token_usage over total_token_usage because total is a
// lifetime counter, not current context usage.
func parseTokenCountInfo(info json.RawMessage, a *accumulator) {
	var nested struct {
		LastTokenUsage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"last_token_usage"`
		ModelContextWindow int `json:"model_context_window"`
	}
	if json.Unmarshal(info, &nested) != nil {
		return
	}
	if nested.LastTokenUsage != nil {
		a.contextTokens = nested.LastTokenUsage.InputTokens
		a.outputTokens = nested.LastTokenUsage.OutputTokens
	}
	if nested.ModelContextWindow > 0 {
		a.maxContextTokens = nested.ModelContextWindow
	}
}

// parseTokenCountFlat handles the flat token_count format (older Codex CLI
// or bare-format lines).
func parseTokenCountFlat(payload json.RawMessage, a *accumulator) {
	var flat struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		ModelContextWindow int `json:"model_context_window"`
	}
	if json.Unmarshal(payload, &flat) != nil {
		return
	}
	if flat.InputTokens > 0 {
		a.contextTokens = flat.InputTokens
	}
	if flat.OutputTokens > 0 {
		a.outputTokens = flat.OutputTokens
	}
	if flat.ModelContextWindow > 0 {
		a.maxContextTokens = flat.ModelContextWindow
	}
}

// parseModel decodes a model value that may be a plain string or an object
// with name/id/model fields.
func parseModel(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Name  string `json:"name"`
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return firstNonEmpty(obj.Name, obj.ID, obj.Model)
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
