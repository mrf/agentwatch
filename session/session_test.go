package session_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
)

// --- Activity marshal round-trip ---

func TestActivity_MarshalRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a    session.Activity
	}{
		{"idle", session.ActivityIdle},
		{"working", session.ActivityWorking},
		{"waiting", session.ActivityWaiting},
		{"terminal", session.ActivityTerminal},
		{"unknown forward-compat", session.Activity("unknown-future-value")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tc.a)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got session.Activity
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != tc.a {
				t.Errorf("round-trip: got %q, want %q", got, tc.a)
			}
		})
	}
}

func TestActivity_UnmarshalBadType(t *testing.T) {
	t.Parallel()
	var a session.Activity
	if err := json.Unmarshal([]byte(`123`), &a); err == nil {
		t.Error("expected error unmarshaling non-string JSON, got nil")
	}
}

// --- SessionState clone independence ---

func TestSessionState_CloneIndependence(t *testing.T) {
	t.Parallel()

	now := time.Now()
	completed := now.Add(time.Hour)
	original := session.SessionState{
		ID:       "sess-1",
		Source:   "claude",
		Activity: session.ActivityWorking,
		Lifecycle: session.LifecycleActive,
		CompletedAt: &completed,
		Subagents: []session.SubagentState{
			{
				ID:             "sub-1",
				Activity:       session.ActivityWorking,
				StartedAt:      now,
				LastActivityAt: now,
			},
		},
	}

	clone := original.Clone()

	// Mutate clone fields — original must not change.
	clone.ID = "mutated"
	clone.Activity = session.ActivityIdle
	*clone.CompletedAt = now.Add(2 * time.Hour)
	clone.Subagents[0].ID = "sub-mutated"

	if original.ID != "sess-1" {
		t.Errorf("original.ID changed: got %q", original.ID)
	}
	if original.Activity != session.ActivityWorking {
		t.Errorf("original.Activity changed: got %q", original.Activity)
	}
	if original.CompletedAt.Equal(*clone.CompletedAt) {
		t.Errorf("clone and original share CompletedAt pointer")
	}
	if original.Subagents[0].ID != "sub-1" {
		t.Errorf("original.Subagents[0].ID changed: got %q", original.Subagents[0].ID)
	}
}

func TestSessionState_CloneNilCompletedAt(t *testing.T) {
	t.Parallel()
	s := session.SessionState{ID: "x"}
	c := s.Clone()
	if c.CompletedAt != nil {
		t.Error("clone.CompletedAt should be nil when original is nil")
	}
}

func TestSessionState_CloneEmptySubagents(t *testing.T) {
	t.Parallel()
	s := session.SessionState{Subagents: nil}
	c := s.Clone()
	if c.Subagents != nil {
		t.Error("clone.Subagents should be nil when original is nil")
	}
}

// --- ComputeContextUtilization ---

func TestComputeContextUtilization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tokens  int
		max     int
		want    float64
	}{
		{"zero max", 100, 0, 0},
		{"negative max", 100, -1, 0},
		{"half", 50, 100, 0.5},
		{"full", 100, 100, 1.0},
		{"over", 110, 100, 1.1},
		{"zero tokens", 0, 200, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := session.ComputeContextUtilization(tc.tokens, tc.max)
			if got != tc.want {
				t.Errorf("ComputeContextUtilization(%d, %d) = %v, want %v",
					tc.tokens, tc.max, got, tc.want)
			}
		})
	}
}

// --- LifecycleState string values ---

func TestLifecycleState_StringValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state session.LifecycleState
		want  string
	}{
		{session.LifecycleActive, "active"},
		{session.LifecycleTerminal, "terminal"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			if string(tc.state) != tc.want {
				t.Errorf("LifecycleState %q: got %q, want %q", tc.state, string(tc.state), tc.want)
			}
		})
	}
}

// --- LifecycleEventType string values ---

func TestLifecycleEventType_StringValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		et   session.LifecycleEventType
		want string
	}{
		{session.EventDiscovered, "discovered"},
		{session.EventUpdated, "updated"},
		{session.EventResumed, "resumed"},
		{session.EventTerminal, "terminal"},
		{session.EventStale, "stale"},
		{session.EventRemoved, "removed"},
	}
	for _, tc := range cases {
		t.Run(string(tc.et), func(t *testing.T) {
			t.Parallel()
			if string(tc.et) != tc.want {
				t.Errorf("LifecycleEventType %q: got %q, want %q", tc.et, string(tc.et), tc.want)
			}
		})
	}
}

// --- LifecycleEvent JSON round-trip ---

func TestLifecycleEvent_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	ev := session.LifecycleEvent{
		Type:      session.EventDiscovered,
		SessionID: "sess-1",
		Source:    "claude",
		From:      session.LifecycleActive,
		To:        session.LifecycleTerminal,
		At:        now,
		Reason:    "test",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got session.LifecycleEvent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != ev.Type || got.SessionID != ev.SessionID || got.Source != ev.Source ||
		got.From != ev.From || got.To != ev.To || !got.At.Equal(ev.At) || got.Reason != ev.Reason {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, ev)
	}
}
