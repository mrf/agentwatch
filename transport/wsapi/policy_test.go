package wsapi_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// Sentinel values that must never appear on the wire once redacted.
const (
	secretID         = "sess-secret-id"
	secretSlug       = "secret-slug"
	secretWorkingDir = "/home/user/projects/secret"
	secretBranch     = "feat/secret-branch"
	secretModel      = "secret-model"
	secretSubID      = "sub-secret-id"
	secretSubSlug    = "sub-secret-slug"
	secretSubModel   = "sub-secret-model"
	policySource     = "policy-source"
)

var fullPolicy = session.Policy{
	RedactWorkingDir: true,
	RedactBranch:     true,
	RedactModel:      true,
	RedactSessionID:  true,
	RedactSource:     true,
}

func secretSubagents() []session.SubagentState {
	return []session.SubagentState{{
		ID:       secretSubID,
		ParentID: secretID,
		Slug:     secretSubSlug,
		Model:    secretSubModel,
		Activity: session.ActivityWorking,
	}}
}

func secretState() session.SessionState {
	return session.SessionState{
		ID:         secretID,
		Source:     policySource,
		Slug:       secretSlug,
		Activity:   session.ActivityWorking,
		Model:      secretModel,
		WorkingDir: secretWorkingDir,
		Branch:     secretBranch,
		Subagents:  secretSubagents(),
	}
}

// newPolicyServer builds a server whose monitor holds one session with every
// sensitive field populated.
func newPolicyServer(t *testing.T, opts ...wsapi.Option) (*wsapi.Server, *httptest.Server) {
	t.Helper()

	src := mock.New(
		mock.WithName(policySource),
		mock.WithHandles(source.SessionHandle{ID: secretID, Source: policySource}),
	)
	src.QueueParseResult(secretID, source.SourceUpdate{
		SessionID:  secretID,
		Slug:       secretSlug,
		Activity:   session.ActivityWorking,
		Model:      secretModel,
		WorkingDir: secretWorkingDir,
		Branch:     secretBranch,
		Subagents:  secretSubagents(),
	}, "cursor-1", nil)

	mon, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	srv, err := wsapi.NewServer(mon, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Stop()
		ts.Close()
	})
	return srv, ts
}

// readRaw reads one raw message from the WebSocket.
func readRaw(t *testing.T, ts *httptest.Server, srv *wsapi.Server, ev *monitor.Event) []byte {
	t.Helper()
	conn := dialWS(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First message is always the snapshot.
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read snapshot: %v", err)
	}
	if ev == nil {
		return data
	}
	if err := srv.HandleEvent(context.Background(), *ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read event: %v", err)
	}
	return data
}

func decode(t *testing.T, data []byte) monitor.Event {
	t.Helper()
	var ev monitor.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("Unmarshal: %v (data: %s)", err, data)
	}
	return ev
}

// assertStateRedacted checks each field of a state against the full policy.
func assertStateRedacted(t *testing.T, where string, s session.SessionState) {
	t.Helper()
	checks := []struct {
		field, got string
	}{
		{"ID", s.ID},
		{"Slug", s.Slug},
		{"Source", s.Source},
		{"Model", s.Model},
		{"WorkingDir", s.WorkingDir},
		{"Branch", s.Branch},
	}
	for i := 0; i < len(checks); i++ {
		if checks[i].got != "" {
			t.Errorf("%s.%s not redacted: %q", where, checks[i].field, checks[i].got)
		}
	}
	if len(s.Subagents) != 1 {
		t.Fatalf("%s: expected 1 subagent, got %d", where, len(s.Subagents))
	}
	sub := s.Subagents[0]
	subChecks := []struct {
		field, got string
	}{
		{"ID", sub.ID},
		{"ParentID", sub.ParentID},
		{"Slug", sub.Slug},
		{"Model", sub.Model},
	}
	for i := 0; i < len(subChecks); i++ {
		if subChecks[i].got != "" {
			t.Errorf("%s.Subagents[0].%s not redacted: %q", where, subChecks[i].field, subChecks[i].got)
		}
	}
	// Non-sensitive fields survive.
	if s.Activity != session.ActivityWorking {
		t.Errorf("%s.Activity should survive redaction: got %q", where, s.Activity)
	}
}

// assertNoSecrets checks the raw wire bytes for any sentinel value.
func assertNoSecrets(t *testing.T, data []byte) {
	t.Helper()
	secrets := []string{
		secretID, secretSlug, secretWorkingDir, secretBranch, secretModel,
		secretSubID, secretSubSlug, secretSubModel, policySource,
	}
	raw := string(data)
	for i := 0; i < len(secrets); i++ {
		if strings.Contains(raw, secrets[i]) {
			t.Errorf("wire payload leaks %q: %s", secrets[i], raw)
		}
	}
}

func TestPolicy_Snapshot(t *testing.T) {
	srv, ts := newPolicyServer(t, wsapi.WithPolicy(fullPolicy))
	data := readRaw(t, ts, srv, nil)

	ev := decode(t, data)
	if ev.Type != monitor.EventSnapshot {
		t.Fatalf("expected snapshot, got %s", ev.Type)
	}
	if len(ev.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(ev.Sessions))
	}
	assertStateRedacted(t, "Sessions[0]", ev.Sessions[0])
	assertNoSecrets(t, data)
}

func TestPolicy_Delta(t *testing.T) {
	srv, ts := newPolicyServer(t, wsapi.WithPolicy(fullPolicy))
	in := monitor.Event{
		Seq:     1,
		At:      time.Now(),
		Type:    monitor.EventDelta,
		Updates: []session.SessionState{secretState()},
		Removed: []string{secretID, secretSubID},
	}
	data := readRaw(t, ts, srv, &in)

	ev := decode(t, data)
	if ev.Type != monitor.EventDelta {
		t.Fatalf("expected delta, got %s", ev.Type)
	}
	if len(ev.Updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ev.Updates))
	}
	assertStateRedacted(t, "Updates[0]", ev.Updates[0])
	for i := 0; i < len(ev.Removed); i++ {
		if ev.Removed[i] != "" {
			t.Errorf("Removed[%d] not redacted: %q", i, ev.Removed[i])
		}
	}
	assertNoSecrets(t, data)
}

func TestPolicy_Lifecycle(t *testing.T) {
	srv, ts := newPolicyServer(t, wsapi.WithPolicy(fullPolicy))
	in := monitor.Event{
		Seq:  1,
		At:   time.Now(),
		Type: monitor.EventLifecycle,
		Lifecycle: &session.LifecycleEvent{
			Type:      session.EventDiscovered,
			SessionID: secretID,
			Source:    policySource,
			From:      session.LifecycleActive,
			To:        session.LifecycleActive,
			At:        time.Now(),
		},
	}
	data := readRaw(t, ts, srv, &in)

	ev := decode(t, data)
	if ev.Lifecycle == nil {
		t.Fatalf("expected lifecycle payload: %s", data)
	}
	if ev.Lifecycle.SessionID != "" {
		t.Errorf("Lifecycle.SessionID not redacted: %q", ev.Lifecycle.SessionID)
	}
	if ev.Lifecycle.Source != "" {
		t.Errorf("Lifecycle.Source not redacted: %q", ev.Lifecycle.Source)
	}
	if ev.Lifecycle.Type != session.EventDiscovered {
		t.Errorf("Lifecycle.Type should survive: got %q", ev.Lifecycle.Type)
	}
	assertNoSecrets(t, data)
}

// Each Policy flag clears only its own fields.
func TestPolicy_PerFlag(t *testing.T) {
	cases := []struct {
		name    string
		policy  session.Policy
		cleared []string
	}{
		{"WorkingDir", session.Policy{RedactWorkingDir: true}, []string{secretWorkingDir}},
		{"Branch", session.Policy{RedactBranch: true}, []string{secretBranch}},
		{"Model", session.Policy{RedactModel: true}, []string{secretModel, secretSubModel}},
		{"SessionID", session.Policy{RedactSessionID: true}, []string{secretID, secretSlug, secretSubID, secretSubSlug}},
		{"Source", session.Policy{RedactSource: true}, []string{`"source":"` + policySource}},
	}
	all := []string{secretWorkingDir, secretBranch, secretModel, secretSubModel,
		secretID, secretSlug, secretSubID, secretSubSlug, `"source":"` + policySource}

	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := newPolicyServer(t, wsapi.WithPolicy(tc.policy))
			for _, typ := range []monitor.EventType{monitor.EventSnapshot, monitor.EventDelta} {
				var data []byte
				if typ == monitor.EventSnapshot {
					data = readRaw(t, ts, srv, nil)
				} else {
					in := monitor.Event{Seq: 1, Type: monitor.EventDelta, Updates: []session.SessionState{secretState()}}
					data = readRaw(t, ts, srv, &in)
				}
				raw := string(data)
				for j := 0; j < len(all); j++ {
					want := true
					for k := 0; k < len(tc.cleared); k++ {
						if all[j] == tc.cleared[k] {
							want = false
						}
					}
					if got := strings.Contains(raw, all[j]); got != want {
						t.Errorf("%s: contains %q = %v, want %v: %s", typ, all[j], got, want, raw)
					}
				}
			}
		})
	}
}

// Without a policy the transport is a passthrough.
func TestPolicy_DefaultPassthrough(t *testing.T) {
	_, ts := newPolicyServer(t)
	data := readRaw(t, ts, nil, nil)
	ev := decode(t, data)
	if len(ev.Sessions) != 1 || ev.Sessions[0].WorkingDir != secretWorkingDir {
		t.Fatalf("expected unredacted snapshot: %s", data)
	}
}

// Redaction must not mutate the event handed to HandleEvent: other sinks in a
// MultiSink receive the same value.
func TestPolicy_DoesNotMutateInput(t *testing.T) {
	srv, ts := newPolicyServer(t, wsapi.WithPolicy(fullPolicy))
	lc := &session.LifecycleEvent{Type: session.EventUpdated, SessionID: secretID, Source: policySource}
	in := monitor.Event{
		Seq:       1,
		Type:      monitor.EventDelta,
		Updates:   []session.SessionState{secretState()},
		Removed:   []string{secretID},
		Lifecycle: lc,
	}
	_ = readRaw(t, ts, srv, &in)

	if in.Updates[0].ID != secretID || in.Updates[0].Subagents[0].Slug != secretSubSlug {
		t.Errorf("Updates mutated: %+v", in.Updates[0])
	}
	if in.Removed[0] != secretID {
		t.Errorf("Removed mutated: %q", in.Removed[0])
	}
	if lc.SessionID != secretID || lc.Source != policySource {
		t.Errorf("Lifecycle mutated: %+v", *lc)
	}
}
