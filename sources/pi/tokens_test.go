package pi

import "testing"

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
