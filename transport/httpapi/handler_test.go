package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
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

	r := httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil)
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

func TestHandler_GetSession_NotFound(t *testing.T) {
	mon, _ := buildMonitor(t)
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/sessions/does-not-exist", nil)
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

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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

	r := httptest.NewRequest(http.MethodGet, "/sources", nil)
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
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
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
	r2 := httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil)
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

	r := httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil)
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

func assertContentTypeJSON(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}
