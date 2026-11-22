package pi

import (
	"testing"

	"github.com/mrf/agentwatch/session"
)

func TestToUpdate_HasTokensOnlyWithUsage(t *testing.T) {
	var acc accumulator
	parseLine([]byte(`{"type":"message","id":"m1","timestamp":"2024-06-01T09:01:00Z","message":{"role":"user"}}`), false, &acc)
	if acc.toUpdate().HasTokens {
		t.Error("user-only batch: HasTokens = true, want false")
	}
	parseLine([]byte(`{"type":"message","id":"m2","parentId":"m1","timestamp":"2024-06-01T09:02:00Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input":500,"output":100,"cacheRead":0,"cacheWrite":0},"stopReason":"end_turn"}}`), false, &acc)
	if !acc.toUpdate().HasTokens {
		t.Error("batch with assistant usage: HasTokens = false, want true")
	}
}

func TestParseMessage_CostShapes(t *testing.T) {
	cases := []struct {
		name string
		cost string
	}{
		{"object", `,"cost":{"input":0.1,"output":0.2,"cacheRead":0,"cacheWrite":0,"total":0.3}`},
		{"number", `,"cost":0.3`},
		{"absent", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var acc accumulator
			parseMessage([]byte(`{"role":"assistant","model":"m","stopReason":"stop","usage":{"input":10,"output":7,"cacheRead":3,"cacheWrite":2`+tc.cost+`}}`), &acc)
			if acc.messageDelta != 1 || !acc.hasTokens || acc.model != "m" {
				t.Fatalf("messageDelta=%d hasTokens=%v model=%q, want 1 true m", acc.messageDelta, acc.hasTokens, acc.model)
			}
			if acc.contextTokens != 15 || acc.outputTokens != 7 {
				t.Errorf("context=%d output=%d, want 15 7", acc.contextTokens, acc.outputTokens)
			}
		})
	}
}

func TestParseLines_ObjectCostFixture(t *testing.T) {
	acc := runLines(fixtureLines(t, "cost_object.jsonl"))
	if acc.messageDelta != 2 || acc.model != "claude-opus-4-6" || !acc.hasTokens {
		t.Fatalf("messageDelta=%d model=%q hasTokens=%v, want 2 claude-opus-4-6 true", acc.messageDelta, acc.model, acc.hasTokens)
	}
	// context = input(120) + cacheRead(40) + cacheWrite(10)
	if acc.contextTokens != 170 || acc.outputTokens != 30 {
		t.Errorf("context=%d output=%d, want 170 30", acc.contextTokens, acc.outputTokens)
	}
}

func TestParseMessage_UnreadableUsageKeepsMessage(t *testing.T) {
	var acc accumulator
	parseMessage([]byte(`{"role":"assistant","model":"m","stopReason":"stop","usage":{"input":"many"}}`), &acc)
	if acc.messageDelta != 1 || acc.model != "m" || acc.activity != session.ActivityWaiting {
		t.Errorf("messageDelta=%d model=%q activity=%v, want message counted despite bad usage", acc.messageDelta, acc.model, acc.activity)
	}
	if acc.hasTokens {
		t.Error("hasTokens = true for unreadable usage, want false")
	}
}
