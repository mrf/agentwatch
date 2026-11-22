package monitor_test

import (
	"testing"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

func TestApplyUpdate_ResetReplacesCounts(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.SourceUpdate{MessageCountDelta: 5, ToolCallCountDelta: 3, CompactionCountDelta: 2},
		source.SourceUpdate{MessageCountDelta: 1},
		source.SourceUpdate{Reset: true, MessageCountDelta: 4, ToolCallCountDelta: 1},
	)
	if s.MessageCount != 4 || s.ToolCallCount != 1 || s.CompactionCount != 0 {
		t.Errorf("counts = msg %d tool %d compact %d, want 4/1/0 (reset replaces)", s.MessageCount, s.ToolCallCount, s.CompactionCount)
	}
}

func TestApplyUpdate_ResetThenDeltasAccumulate(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.SourceUpdate{MessageCountDelta: 9},
		source.SourceUpdate{Reset: true, MessageCountDelta: 2},
		source.SourceUpdate{MessageCountDelta: 3},
	)
	if s.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", s.MessageCount)
	}
}

func TestApplyUpdate_ResetReplacesSubagents(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.SourceUpdate{MessageCountDelta: 1, Subagents: []session.SubagentState{
			{ID: "old", MessageCount: 7},
			{ID: "kept", MessageCount: 9},
		}},
		source.SourceUpdate{Reset: true, Subagents: []session.SubagentState{
			{ID: "kept", MessageCount: 2},
		}},
	)
	if len(s.Subagents) != 1 || s.Subagents[0].ID != "kept" || s.Subagents[0].MessageCount != 2 {
		t.Errorf("Subagents = %+v, want only kept with MessageCount 2", s.Subagents)
	}
}

func TestApplyUpdate_ResetWithNoSubagentsClearsThem(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.SourceUpdate{MessageCountDelta: 1, Subagents: []session.SubagentState{{ID: "a", MessageCount: 1}}},
		source.SourceUpdate{Reset: true},
	)
	if len(s.Subagents) != 0 || s.MessageCount != 0 {
		t.Errorf("after empty reset: subagents=%+v msg=%d, want none/0", s.Subagents, s.MessageCount)
	}
}
