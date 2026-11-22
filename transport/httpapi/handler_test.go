package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/httpapi"
)

// buildMonitor creates a monitor with a single mock source and one populated session.
func buildMonitor(t *testing.T) (*monitor.Monitor, string) {
	t.Helper()

	sessionID := "test-session-1"
	src := mock.New(
		mock.WithName("mock"),
		mock.WithHandles(source.SessionHandle{
			ID:        sessionID,
			Source:    "mock",
			StartedAt: time.Now(),
		}),
		mock.WithParseResult(sessionID, source.SourceUpdate{
			SessionID:  sessionID,
			Activity:   session.ActivityWorking,
			Model:      "test-model",
			WorkingDir: "/home/user/project",
			Branch:     "main",
		}, "", nil),
	)

	mon, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	return mon, sessionID
}

func TestHandler_ListSessions(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertContentTypeJSON(t, w)

	var states []session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&states); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 session, got %d", len(states))
	}
	if states[0].Activity != session.ActivityWorking {
		t.Errorf("expected ActivityWorking, got %q", states[0].Activity)
	}
}

func TestHandler_GetSession(t *testing.T) {
	mon, id := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/sessions/"+id, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertContentTypeJSON(t, w)

	var s session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ID != id {
		t.Errorf("expected ID %q, got %q", id, s.ID)
	}
	if s.Model != "test-model" {
		t.Errorf("expected model %q, got %q", "test-model", s.Model)
	}
}

// With a Pseudonymizer, the IDs a client sees in /sessions can be fed back
// into /sessions/{id}, and the real ID never appears in either response.
func TestHandler_Pseudonymize_ListThenGet(t *testing.T) {
	mon, id := buildMonitor(t)
	z, err := session.NewPseudonymizer(session.WithPseudonymKey([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatalf("NewPseudonymizer: %v", err)
	}
	h, err := httpapi.NewHandler(mon, httpapi.WithPolicy(session.Policy{Pseudonymizer: z}))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLocalRequest("/sessions", nil))
	if strings.Contains(w.Body.String(), id) {
		t.Fatalf("list leaks real ID: %s", w.Body.String())
	}
	var states []session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&states); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(states) != 1 || states[0].ID != z.Pseudonym(id) {
		t.Fatalf("expected one pseudonymous session, got %+v", states)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, newLocalRequest("/sessions/"+states[0].ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET by pseudonym: expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), id) {
		t.Fatalf("get leaks real ID: %s", w.Body.String())
	}
	var s session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ID != states[0].ID {
		t.Errorf("GET ID %q does not match list ID %q", s.ID, states[0].ID)
	}
}

func TestHandler_GetSession_NotFound(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/sessions/does-not-exist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	assertContentTypeJSON(t, w)

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected non-empty error field in 404 response")
	}
}

func TestHandler_Healthz_Empty(t *testing.T) {
	// Monitor with no polls yet — health map is empty.
	src := mock.New(mock.WithName("mock"))
	mon, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertContentTypeJSON(t, w)

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["status"]; !ok {
		t.Error("expected 'status' field in healthz response")
	}
	if _, ok := resp["sources"]; !ok {
		t.Error("expected 'sources' field in healthz response")
	}
}

func TestHandler_Healthz_AfterPoll(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status  string           `json:"status"`
		Sources []monitor.Health `json:"sources"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != string(monitor.HealthHealthy) {
		t.Errorf("expected healthy status, got %q", resp.Status)
	}
	if len(resp.Sources) != 1 {
		t.Errorf("expected 1 source health record, got %d", len(resp.Sources))
	}
	if resp.Sources[0].Source != "mock" {
		t.Errorf("expected source 'mock', got %q", resp.Sources[0].Source)
	}
}

// buildMonitorWithStatus polls a monitor whose only source ends in the given
// health status (threshold 1: one discover failure degrades, two fail).
func buildMonitorWithStatus(t *testing.T, want monitor.HealthStatus) *monitor.Monitor {
	t.Helper()
	src := mock.New(mock.WithName("mock"))
	mon, err := monitor.New(monitor.WithSources(src), monitor.WithHealthThreshold(1))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	failures := map[monitor.HealthStatus]int{
		monitor.HealthHealthy:  0,
		monitor.HealthDegraded: 1,
		monitor.HealthFailed:   2,
	}[want]
	if failures == 0 {
		if err := mon.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	for i := 0; i < failures; i++ {
		src.QueueDiscoverError(io.ErrUnexpectedEOF)
		if err := mon.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	if got := mon.Health()["mock"].Status; got != want {
		t.Fatalf("setup: source status = %q, want %q", got, want)
	}
	return mon
}

func TestHandler_Healthz_StatusCode(t *testing.T) {
	tests := []struct {
		status   monitor.HealthStatus
		wantCode int
	}{
		{monitor.HealthHealthy, http.StatusOK},
		{monitor.HealthDegraded, http.StatusOK},
		{monitor.HealthFailed, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			h, err := httpapi.NewHandler(buildMonitorWithStatus(t, tt.status))
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newLocalRequest("/healthz", nil))

			if w.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tt.wantCode)
			}
			assertContentTypeJSON(t, w)
			var resp struct {
				Status string `json:"status"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != string(tt.status) {
				t.Errorf("body status = %q, want %q", resp.Status, tt.status)
			}
		})
	}
}

func TestHandler_Sources(t *testing.T) {
	src := mock.New(mock.WithName("testSource"))
	mon, err := monitor.New(monitor.WithSources(src))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/sources", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertContentTypeJSON(t, w)

	var resp struct {
		Sources []string `json:"sources"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sources) != 1 || resp.Sources[0] != "testSource" {
		t.Errorf("expected [testSource], got %v", resp.Sources)
	}
}

func TestHandler_PrivacyFilter_RedactsFields(t *testing.T) {
	mon, id := buildMonitor(t)

	policy := session.Policy{
		RedactWorkingDir: true,
		RedactBranch:     true,
		RedactModel:      true,
	}
	h, err := httpapi.NewHandler(mon, httpapi.WithPolicy(policy))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// Test via list endpoint.
	r := newLocalRequest("/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var states []session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&states); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(states) == 0 {
		t.Fatal("expected at least one session")
	}
	s := states[0]
	if s.WorkingDir != "" {
		t.Errorf("WorkingDir should be redacted, got %q", s.WorkingDir)
	}
	if s.Branch != "" {
		t.Errorf("Branch should be redacted, got %q", s.Branch)
	}
	if s.Model != "" {
		t.Errorf("Model should be redacted, got %q", s.Model)
	}

	// Test via single session endpoint — same policy applies.
	r2 := newLocalRequest("/sessions/"+id, nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)

	var s2 session.SessionState
	if err := json.NewDecoder(w2.Body).Decode(&s2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s2.WorkingDir != "" {
		t.Errorf("WorkingDir should be redacted on single session, got %q", s2.WorkingDir)
	}
}

func TestHandler_PrivacyFilter_PassthroughByDefault(t *testing.T) {
	mon, id := buildMonitor(t)
	h, err := httpapi.NewHandler(mon) // no policy — pass-through
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newLocalRequest("/sessions/"+id, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var s session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.WorkingDir == "" {
		t.Error("WorkingDir should not be redacted with default (zero) policy")
	}
	if s.Model == "" {
		t.Error("Model should not be redacted with default (zero) policy")
	}
}

func TestHandler_Host_DNSRebindingRejected(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// DNS rebinding: the browser sends the attacker's hostname as Host.
	r := httptest.NewRequest(http.MethodGet, "http://evil.example/sessions", nil)
	r.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "test-model") {
		t.Fatal("rejected response must not contain session data")
	}
}

func TestHandler_Host_LocalhostAccepted(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	hosts := []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"}
	for i := 0; i < len(hosts); i++ {
		r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		r.Host = hosts[i]
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("Host %q: expected 200, got %d", hosts[i], w.Code)
		}
	}
}

func TestHandler_Host_AllowedHostAccepted(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon, httpapi.WithAllowedHosts([]string{"agentwatch.lan"}))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "http://agentwatch.lan:8080/sessions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "http://evil.example/sessions", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unconfigured host: expected 403, got %d", w.Code)
	}
}

// newLocalRequest builds a GET request addressed to a loopback Host.
// httptest.NewRequest defaults Host to example.com, which the handler rejects.
func newLocalRequest(target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, body)
	r.Host = "127.0.0.1:8080"
	return r
}

func assertContentTypeJSON(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}
