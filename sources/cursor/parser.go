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
		// User message arrived; agent has not yet responded.
		a.activity = session.ActivityWaiting
	case "assistant":
		// Assistant is generating or has generated a response.
		a.activity = session.ActivityWorking
	}
}
