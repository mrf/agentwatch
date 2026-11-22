package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/httpapi"
)

func getSeqHeader(t *testing.T, w *httptest.ResponseRecorder) uint64 {
	t.Helper()
	raw := w.Header().Get("X-Agentwatch-Seq")
	if raw == "" {
		t.Fatal("X-Agentwatch-Seq header missing")
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("header %q is not a uint64: %v", raw, err)
	}
	return seq
}

func TestHandler_ListSessions_SeqHeader(t *testing.T) {
	const id = "seq-session"
	src := mock.New(
		mock.WithName("mock"),
		mock.WithHandles(source.SessionHandle{ID: id, Source: "mock", StartedAt: time.Now()}),
		mock.WithParseResult(id, source.SourceUpdate{
			SessionID: id,
			Activity:  session.ActivityWorking,
			Model:     "test-model",
		}, "", nil),
	)

	var mu sync.Mutex
	var events []monitor.Event
	sink := monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
		return nil
	})
	mon, err := monitor.New(monitor.WithSources(src), monitor.WithSink(sink))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLocalRequest("/sessions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("test setup: expected events from PollOnce")
	}
	last := events[len(events)-1].Seq
	if got := getSeqHeader(t, w); got != last {
		t.Errorf("header seq = %d, want last event seq %d", got, last)
	}

	// Body stays a plain JSON array, and already reflects every delta with
	// Seq <= header (so a client drops those).
	var states []session.SessionState
	if err := json.NewDecoder(w.Body).Decode(&states); err != nil {
		t.Fatalf("body is not a JSON array of sessions: %v", err)
	}
	if len(states) != 1 || states[0].ID != id || states[0].Activity != session.ActivityWorking {
		t.Fatalf("body does not reflect delivered events: %+v", states)
	}
}

func TestHandler_ListSessions_SeqHeader_EmptyMonitor(t *testing.T) {
	mon, err := monitor.New(monitor.WithSources(mock.New(mock.WithName("mock"))))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	h, err := httpapi.NewHandler(mon)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLocalRequest("/sessions", nil))
	if got := getSeqHeader(t, w); got != 0 {
		t.Errorf("header = %d, want 0", got)
	}
}
