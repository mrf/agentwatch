package cursor

import (
	"testing"

	"github.com/mrf/agentwatch/session"
)

// ---- parseLine tests --------------------------------------------------------

func TestParseLineUserMessage(t *testing.T) {
	line := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"fix the bug"}]}}`)
	var acc accumulator
	parseLine(line, &acc)

	if acc.messageDelta != 1 {
		t.Errorf("messageDelta = %d, want 1", acc.messageDelta)
	}
	if acc.activity != session.ActivityWaiting {
		t.Errorf("activity = %q, want %q", acc.activity, session.ActivityWaiting)
	}
}

func TestParseLineAssistantMessage(t *testing.T) {
	line := []byte(`{"role":"assistant","message":{"content":[{"type":"text","text":"on it"}]}}`)
	var acc accumulator
	parseLine(line, &acc)

	if acc.messageDelta != 1 {
		t.Errorf("messageDelta = %d, want 1", acc.messageDelta)
	}
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity = %q, want %q", acc.activity, session.ActivityWorking)
	}
}

func TestParseLineMalformedSkipped(t *testing.T) {
	line := []byte(`not valid json{{{`)
	var acc accumulator
	parseLine(line, &acc)

	if acc.messageDelta != 0 {
		t.Errorf("messageDelta = %d, want 0 for malformed line", acc.messageDelta)
	}
}

func TestParseLineNoRoleSkipped(t *testing.T) {
	line := []byte(`{"type":"something","data":"value"}`)
	var acc accumulator
	parseLine(line, &acc)

	if acc.messageDelta != 0 {
		t.Errorf("messageDelta = %d, want 0 for line without role", acc.messageDelta)
	}
}

func TestParseLineEmptyLineSkipped(t *testing.T) {
	var acc accumulator
	parseLine([]byte{}, &acc)

	if acc.messageDelta != 0 {
		t.Errorf("messageDelta = %d, want 0 for empty line", acc.messageDelta)
	}
}

func TestParseLineUnknownRoleCountsMessage(t *testing.T) {
	// Unknown roles still increment messageDelta but don't change activity.
	line := []byte(`{"role":"system","message":{"content":[]}}`)
	var acc accumulator
	parseLine(line, &acc)

	if acc.messageDelta != 1 {
		t.Errorf("messageDelta = %d, want 1 for unknown role", acc.messageDelta)
	}
	if acc.activity != "" {
		t.Errorf("activity = %q, want empty for unknown role", acc.activity)
	}
}

// ---- accumulator tests ------------------------------------------------------

func TestAccumulatorMultipleLines(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`),
		[]byte(`{"role":"assistant","message":{"content":[{"type":"text","text":"hi there"}]}}`),
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"thanks"}]}}`),
	}

	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], &acc)
	}

	if acc.messageDelta != 3 {
		t.Errorf("messageDelta = %d, want 3", acc.messageDelta)
	}
	// Last role was user → waiting.
	if acc.activity != session.ActivityWaiting {
		t.Errorf("activity = %q, want %q", acc.activity, session.ActivityWaiting)
	}
}

func TestAccumulatorLastRoleWins(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"role":"user","message":{"content":[]}}`),
		[]byte(`{"role":"assistant","message":{"content":[]}}`),
	}

	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], &acc)
	}

	// Last role was assistant → working.
	if acc.activity != session.ActivityWorking {
		t.Errorf("activity = %q, want %q", acc.activity, session.ActivityWorking)
	}
}

func TestAccumulatorToUpdate(t *testing.T) {
	var acc accumulator
	acc.messageDelta = 4
	acc.activity = session.ActivityWaiting

	u := acc.toUpdate()
	if u.MessageCountDelta != 4 {
		t.Errorf("MessageCountDelta = %d, want 4", u.MessageCountDelta)
	}
	if u.Activity != session.ActivityWaiting {
		t.Errorf("Activity = %q, want %q", u.Activity, session.ActivityWaiting)
	}
}

// ---- decodeProjectName tests -------------------------------------------------

func TestDecodeProjectName(t *testing.T) {
	cases := []struct {
		encoded string
		want    string
	}{
		{"Users-john-Code-myapp", "/Users/john/Code/myapp"},
		{"home-mrf-Projects-agentwatch", "/home/mrf/Projects/agentwatch"},
		{"myproject", "/myproject"},
		{"", ""},
	}

	for _, c := range cases {
		got := decodeProjectName(c.encoded)
		if got != c.want {
			t.Errorf("decodeProjectName(%q) = %q, want %q", c.encoded, got, c.want)
		}
	}
}
