package wsapi

import (
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
)

// applyPolicy returns a copy of ev with p applied to every session-bearing
// field. The input is never mutated: a MultiSink hands the same Event value
// (sharing slices and the Lifecycle pointer) to every sink. Health events
// carry no session data and pass through unchanged.
func applyPolicy(p session.Policy, ev monitor.Event) monitor.Event {
	if p == (session.Policy{}) {
		return ev
	}

	ev.Sessions = applyPolicyStates(p, ev.Sessions)
	ev.Updates = applyPolicyStates(p, ev.Updates)

	if p.RedactSessionID && ev.Removed != nil {
		// Keep the count so clients can tell removals happened, but drop
		// the identifiers.
		ev.Removed = make([]string, len(ev.Removed))
	}

	if ev.Lifecycle != nil {
		lc := *ev.Lifecycle
		if p.RedactSessionID {
			lc.SessionID = ""
		}
		if p.RedactSource {
			lc.Source = ""
		}
		ev.Lifecycle = &lc
	}

	return ev
}

func applyPolicyStates(p session.Policy, states []session.SessionState) []session.SessionState {
	if states == nil {
		return nil
	}
	out := make([]session.SessionState, len(states))
	for i := 0; i < len(states); i++ {
		out[i] = p.Apply(states[i])
	}
	return out
}
