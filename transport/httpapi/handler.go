// Package httpapi provides an optional HTTP transport adapter for agentwatch.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
)

// Option configures a Handler.
type Option func(*handler)

// WithPolicy sets the privacy policy applied to all session responses.
// A zero-value Policy is a pass-through — nothing is redacted.
func WithPolicy(p session.Policy) Option {
	return func(h *handler) {
		h.policy = p
	}
}

type handler struct {
	mon    *monitor.Monitor
	policy session.Policy
	mux    *http.ServeMux
}

// NewHandler returns an http.Handler that serves the agentwatch HTTP API.
// Routes:
//
//	GET /sessions       — list all sessions (Snapshot)
//	GET /sessions/{id}  — single session by ID (404 if not found)
//	GET /healthz        — source health status
//	GET /sources        — registered source names
//
// All responses are JSON with Content-Type: application/json.
// The optional privacy policy is applied to every session response.
func NewHandler(mon *monitor.Monitor, opts ...Option) (http.Handler, error) {
	h := &handler{
		mon: mon,
		mux: http.NewServeMux(),
	}
	for _, o := range opts {
		o(h)
	}

	h.mux.HandleFunc("GET /sessions", h.listSessions)
	h.mux.HandleFunc("GET /sessions/{id}", h.getSession)
	h.mux.HandleFunc("GET /healthz", h.getHealthz)
	h.mux.HandleFunc("GET /sources", h.getSources)

	return h.mux, nil
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	states := h.mon.Snapshot()
	for i := 0; i < len(states); i++ {
		states[i] = h.policy.Apply(states[i])
	}
	writeJSON(w, http.StatusOK, states)
}

func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, ok := h.mon.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, h.policy.Apply(s))
}

type healthzResponse struct {
	Status  monitor.HealthStatus `json:"status"`
	Sources []monitor.Health     `json:"sources"`
}

func (h *handler) getHealthz(w http.ResponseWriter, r *http.Request) {
	healthMap := h.mon.Health()

	sources := make([]monitor.Health, 0, len(healthMap))
	overall := monitor.HealthHealthy
	for _, v := range healthMap {
		sources = append(sources, v)
		if v.Status == monitor.HealthFailed {
			overall = monitor.HealthFailed
		} else if v.Status == monitor.HealthDegraded && overall != monitor.HealthFailed {
			overall = monitor.HealthDegraded
		}
	}

	writeJSON(w, http.StatusOK, healthzResponse{
		Status:  overall,
		Sources: sources,
	})
}

type sourcesResponse struct {
	Sources []string `json:"sources"`
}

func (h *handler) getSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sourcesResponse{Sources: h.mon.Sources()})
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
