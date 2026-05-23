package antigravity

import (
	"encoding/json"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// sessionFile is the top-level structure of an Antigravity CLI session file.
// Antigravity rewrites this file in full on every update; it is not append-only.
//
// The schema is inferred from Antigravity 2.0 (launched 2026-05-19) which
// evolves from the Gemini CLI checkpoint format. Field names follow observed
// patterns: "messages" replaces "conversationHistory", token count fields
// use "inputTokenCount"/"outputTokenCount" instead of the Gemini-era
// "promptTokenCount"/"candidatesTokenCount".
type sessionFile struct {
	SessionID string    `json:"sessionId"`
	Messages  []message `json:"messages"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
}

// message is one turn in the conversation (a "user" or "model" message).
type message struct {
	Role          string         `json:"role"`
	Parts         []part         `json:"parts"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
}

// part is one element of a message's parts array.
// Only one of Text, FunctionCall, or FunctionResponse is populated.
type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionCall struct {
	Name string `json:"name"`
}

type functionResponse struct {
	Name string `json:"name"`
}

type usageMetadata struct {
	InputTokenCount  int `json:"inputTokenCount"`
	OutputTokenCount int `json:"outputTokenCount"`
	TotalTokenCount  int `json:"totalTokenCount"`
}

// parseResult holds extracted monitoring state from a parsed session file.
type parseResult struct {
	model string

	// totalUserMsgs counts text-bearing user turns (not functionResponse turns).
	totalUserMsgs int
	// totalModelMsgs counts model turns.
	totalModelMsgs int
	// totalToolCalls counts functionCall parts across all model turns.
	totalToolCalls int

	// currentTool is the name of the last functionCall seen, if any.
	currentTool string

	// contextTokens and outputTokens from the latest model usageMetadata.
	contextTokens int
	outputTokens  int

	// activity is derived from the last turn.
	activity session.Activity
}

// parseSession parses an Antigravity CLI session JSON file and returns a
// parseResult. It never returns an error for recoverable format issues
// (unknown fields, missing optional parts) — only for unparseable JSON.
func parseSession(data []byte) (parseResult, error) {
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return parseResult{}, err
	}

	r := parseResult{
		model:    sf.Model,
		activity: session.ActivityIdle,
	}

	for i := 0; i < len(sf.Messages); i++ {
		msg := sf.Messages[i]
		switch msg.Role {
		case "user":
			if isTextTurn(msg) {
				r.totalUserMsgs++
			}
			// functionResponse turns indicate the model is processing a tool result.
			if hasFunctionResponse(msg) {
				r.activity = session.ActivityWorking
			}

		case "model":
			r.totalModelMsgs++

			// Count function calls and record the latest tool name.
			for j := 0; j < len(msg.Parts); j++ {
				if msg.Parts[j].FunctionCall != nil {
					r.totalToolCalls++
					r.currentTool = msg.Parts[j].FunctionCall.Name
				}
			}

			// Capture token counts from the last model turn that carries them.
			if msg.UsageMetadata != nil {
				r.contextTokens = msg.UsageMetadata.InputTokenCount
				r.outputTokens = msg.UsageMetadata.OutputTokenCount
			}

			// Activity after a model turn: waiting if it ended with text,
			// working if it ended with a tool call.
			if lastPartIsFunctionCall(msg) {
				r.activity = session.ActivityWorking
			} else {
				r.activity = session.ActivityWaiting
			}
		}
	}

	// A final user text turn means the model is actively processing.
	if len(sf.Messages) > 0 {
		last := sf.Messages[len(sf.Messages)-1]
		if last.Role == "user" && isTextTurn(last) {
			r.activity = session.ActivityWorking
		}
	}

	return r, nil
}

// buildUpdate converts a parseResult plus cursor deltas into a SourceUpdate.
// msgDelta and toolDelta are the differences since the previous poll.
func buildUpdate(r parseResult, id, workingDir string, msgDelta, toolDelta int) source.SourceUpdate {
	return source.SourceUpdate{
		SessionID:          id,
		Activity:           r.activity,
		Model:              r.model,
		ContextTokens:      r.contextTokens,
		OutputTokens:       r.outputTokens,
		MessageCountDelta:  msgDelta,
		ToolCallCountDelta: toolDelta,
		CurrentTool:        r.currentTool,
		WorkingDir:         workingDir,
	}
}

// isTextTurn reports whether the message has at least one non-empty text part.
func isTextTurn(msg message) bool {
	for i := 0; i < len(msg.Parts); i++ {
		if msg.Parts[i].Text != "" {
			return true
		}
	}
	return false
}

// hasFunctionResponse reports whether the message contains a functionResponse part.
func hasFunctionResponse(msg message) bool {
	for i := 0; i < len(msg.Parts); i++ {
		if msg.Parts[i].FunctionResponse != nil {
			return true
		}
	}
	return false
}

// lastPartIsFunctionCall reports whether the last part in msg is a functionCall.
func lastPartIsFunctionCall(msg message) bool {
	if len(msg.Parts) == 0 {
		return false
	}
	return msg.Parts[len(msg.Parts)-1].FunctionCall != nil
}
