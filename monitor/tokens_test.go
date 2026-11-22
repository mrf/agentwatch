package monitor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// pollTokens runs one PollOnce per update against a single session and
// returns the session state after the last poll.
func pollTokens(t *testing.T, updates ...source.Update) session.State {
	t.Helper()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{
		{ID: "s1", Source: "test", StartedAt: clk.Now()},
	})
	m, err := monitor.New(monitor.WithSources(src), monitor.WithSink(&recordingSink{}), monitor.WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < len(updates); i++ {
		u := updates[i]
		u.SessionID = "s1"
		src.AddUpdate("s1", u, source.Cursor(fmt.Sprintf("c%d", i)))
		if err := m.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}
	s, ok := m.Get("s1")
	if !ok {
		t.Fatal("session s1 missing")
	}
	return s
}

func TestApplyUpdate_TokensKeptWhenBatchHasNone(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.Update{HasTokens: true, ContextTokens: 5000, OutputTokens: 40, MaxContextTokens: 10000, TokenEstimated: true, MessageCountDelta: 1},
		source.Update{MessageCountDelta: 1},
	)
	if s.ContextTokens != 5000 || s.OutputTokens != 40 || !s.TokenEstimated {
		t.Errorf("tokens = ctx %d out %d est %v, want 5000/40/true", s.ContextTokens, s.OutputTokens, s.TokenEstimated)
	}
	if s.ContextUtilization != 0.5 {
		t.Errorf("ContextUtilization = %v, want 0.5", s.ContextUtilization)
	}
}

func TestApplyUpdate_HasTokensZeroOverwrites(t *testing.T) {
	t.Parallel()
	s := pollTokens(t,
		source.Update{HasTokens: true, ContextTokens: 5000, OutputTokens: 40},
		source.Update{HasTokens: true},
	)
	if s.ContextTokens != 0 || s.OutputTokens != 0 {
		t.Errorf("tokens = ctx %d out %d, want 0/0 (explicit report of zero)", s.ContextTokens, s.OutputTokens)
	}
}

func TestApplyUpdate_NonZeroTokensWithoutFlagStillApply(t *testing.T) {
	t.Parallel()
	s := pollTokens(t, source.Update{ContextTokens: 700, OutputTokens: 9})
	if s.ContextTokens != 700 || s.OutputTokens != 9 {
		t.Errorf("tokens = ctx %d out %d, want 700/9", s.ContextTokens, s.OutputTokens)
	}
}
