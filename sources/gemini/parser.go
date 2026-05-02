package gemini

import (
	"encoding/json"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// checkpoint is the top-level structure of a Gemini CLI checkpoint.json file.
// Gemini rewrites this file in full on every update; it is not append-only.
type checkpoint struct {
	ConversationHistory []contentItem `json:"conversationHistory"`
	Model               string        `json:"model"`
}

// contentItem is one turn in the conversation (a "user" or "model" message).
type contentItem struct {
	Role          string        `json:"role"`
	Parts         []part        `json:"parts"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
}

// part is one element of a content item's parts array.
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
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// parseResult holds extracted monitoring state from a parsed checkpoint.
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

// parseCheckpoint parses a checkpoint.json payload and returns a parseResult.
// It never returns an error for recoverable format issues (unknown fields,
// missing optional parts) — only for unparseable JSON.
func parseCheckpoint(data []byte) (parseResult, error) {
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return parseResult{}, err
	}

	r := parseResult{
		model:    cp.Model,
		activity: session.ActivityIdle,
	}

	for i := 0; i < len(cp.ConversationHistory); i++ {
		item := cp.ConversationHistory[i]
		switch item.Role {
		case "user":
			if isTextTurn(item) {
				r.totalUserMsgs++
			}
			// functionResponse turns indicate the model is processing a tool result.
			if hasFunctionResponse(item) {
				r.activity = session.ActivityWorking
			}

		case "model":
			r.totalModelMsgs++

			// Count function calls and record the latest tool name.
			for j := 0; j < len(item.Parts); j++ {
				if item.Parts[j].FunctionCall != nil {
					r.totalToolCalls++
					r.currentTool = item.Parts[j].FunctionCall.Name
				}
			}

			// Capture token counts from the last model turn that carries them.
			if item.UsageMetadata != nil {
				r.contextTokens = item.UsageMetadata.PromptTokenCount
				r.outputTokens = item.UsageMetadata.CandidatesTokenCount
			}

			// Activity after a model turn: waiting if it ended with text,
			// working if it ended with a tool call.
			if lastPartIsFunctionCall(item) {
				r.activity = session.ActivityWorking
			} else {
				r.activity = session.ActivityWaiting
			}
		}
	}

	// A final user text turn means the model is actively processing.
	if len(cp.ConversationHistory) > 0 {
		last := cp.ConversationHistory[len(cp.ConversationHistory)-1]
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

// isTextTurn reports whether the item has at least one non-empty text part.
func isTextTurn(item contentItem) bool {
	for i := 0; i < len(item.Parts); i++ {
		if item.Parts[i].Text != "" {
			return true
		}
	}
	return false
}

// hasFunctionResponse reports whether the item contains a functionResponse part.
func hasFunctionResponse(item contentItem) bool {
	for i := 0; i < len(item.Parts); i++ {
		if item.Parts[i].FunctionResponse != nil {
			return true
		}
	}
	return false
}

// lastPartIsFunctionCall reports whether the last part in item is a functionCall.
func lastPartIsFunctionCall(item contentItem) bool {
	if len(item.Parts) == 0 {
		return false
	}
	return item.Parts[len(item.Parts)-1].FunctionCall != nil
}
