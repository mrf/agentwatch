package cursor

import (
	"encoding/json"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// accumulator collects incremental fields from JSONL lines.
type accumulator struct {
	activity     session.Activity
	messageDelta int
}

func (a *accumulator) toUpdate() source.SourceUpdate {
	return source.SourceUpdate{
		Activity:          a.activity,
		MessageCountDelta: a.messageDelta,
	}
}

// transcriptLine is the JSON structure of each line in a Cursor agent
// transcript file.
type transcriptLine struct {
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
}

// parseLine dispatches a single JSONL line into the accumulator. Malformed or
// unrecognized lines are silently skipped.
func parseLine(line []byte, a *accumulator) {
	var tl transcriptLine
	if json.Unmarshal(line, &tl) != nil {
		return
	}
	if tl.Role == "" {
		return
	}
	a.messageDelta++
	switch tl.Role {
	case "user":
		a.activity = session.ActivityWorking
	case "assistant":
		// Transcripts carry no stop marker, so a text-only assistant message
		// is read as the end of the turn; one with a tool_use block is not.
		if hasToolUse(tl.Message) {
			a.activity = session.ActivityWorking
		} else {
			a.activity = session.ActivityWaiting
		}
	}
}

func hasToolUse(raw json.RawMessage) bool {
	var m struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for i := 0; i < len(m.Content); i++ {
		if m.Content[i].Type == "tool_use" {
			return true
		}
	}
	return false
}
