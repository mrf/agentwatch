package wsapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// testSession is a convenience for setting up mock source data.
type testSession struct {
	ID       string
	Activity session.Activity
	Model    string
}

// newTestServer creates a Server and httptest.Server for testing.
// Sessions are populated via a mock source and one PollOnce cycle.
func newTestServer(t *testing.T, sessions []testSession, opts ...wsapi.Option) (*wsapi.Server, *httptest.Server) {
	t.Helper()

	handles := make([]source.SessionHandle, len(sessions))
	for i := 0; i < len(sessions); i++ {
		handles[i] = source.SessionHandle{
			ID:     sessions[i].ID,
			Source: "test-source",
		}
	}

	src := mock.New(
		mock.WithName("test-source"),
		mock.WithHandles(handles...),
	)

	// Queue parse results so PollOnce produces real state.
	for i := 0; i < len(sessions); i++ {
		src.QueueParseResult(sessions[i].ID, source.SourceUpdate{
			SessionID: sessions[i].ID,
			Activity:  sessions[i].Activity,
			Model:     sessions[i].Model,
		}, "cursor-1", nil)
	}

	mon, err := monitor.New(
		monitor.WithSources(src),
	)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	// Run one poll to populate the monitor state.
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

// dialWS connects a WebSocket client to the test server.
func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	return dialWSWithOrigin(t, ts, "")
}

// dialWSWithOrigin connects with a specific Origin header.
func dialWSWithOrigin(t *testing.T, ts *httptest.Server, origin string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": {origin}}
	}
	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readEvent reads and decodes a single event from the WebSocket.
func readEvent(t *testing.T, conn *websocket.Conn) monitor.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var ev monitor.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("Unmarshal event: %v (data: %s)", err, data)
	}
	return ev
}

func TestServer_ConnectReceivesSnapshot(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1", Activity: session.ActivityWorking, Model: "claude-4"},
		{ID: "sess-2", Activity: session.ActivityIdle, Model: "gpt-5"},
	}

	_, ts := newTestServer(t, sessions)

	conn := dialWS(t, ts)
	ev := readEvent(t, conn)

	if ev.Type != monitor.EventSnapshot {
		t.Fatalf("expected snapshot event, got %s", ev.Type)
	}
	if len(ev.Sessions) != 2 {
		t.Fatalf("expected 2 sessions in snapshot, got %d", len(ev.Sessions))
	}

	// Verify session data round-trips correctly.
	found := make(map[string]bool)
	for i := 0; i < len(ev.Sessions); i++ {
		found[ev.Sessions[i].ID] = true
	}
	if !found["sess-1"] || !found["sess-2"] {
		t.Fatalf("snapshot missing expected sessions: %v", found)
	}
}

func TestServer_DeltaDelivery(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1", Activity: session.ActivityWorking},
	}

	srv, ts := newTestServer(t, sessions)

	conn := dialWS(t, ts)

	// Consume the initial snapshot.
	ev := readEvent(t, conn)
	if ev.Type != monitor.EventSnapshot {
		t.Fatalf("expected snapshot, got %s", ev.Type)
	}

	// Deliver a delta event through the server's HandleEvent.
	deltaEv := monitor.Event{
		Seq:  1,
		At:   time.Now(),
		Type: monitor.EventDelta,
		Updates: []session.SessionState{
			{ID: "sess-1", Activity: session.ActivityIdle},
		},
	}
	if err := srv.HandleEvent(context.Background(), deltaEv); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Read the delta from the client.
	got := readEvent(t, conn)
	if got.Type != monitor.EventDelta {
		t.Fatalf("expected delta event, got %s", got.Type)
	}
	if len(got.Updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got.Updates))
	}
	if got.Updates[0].ID != "sess-1" {
		t.Fatalf("expected update for sess-1, got %s", got.Updates[0].ID)
	}
}

func TestServer_MultipleClients(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1", Activity: session.ActivityWorking},
	}

	srv, ts := newTestServer(t, sessions)

	// Connect two clients.
	conn1 := dialWS(t, ts)
	conn2 := dialWS(t, ts)

	// Both should get snapshots.
	ev1 := readEvent(t, conn1)
	ev2 := readEvent(t, conn2)
	if ev1.Type != monitor.EventSnapshot || ev2.Type != monitor.EventSnapshot {
		t.Fatal("both clients should receive snapshots")
	}

	// Broadcast a delta.
	deltaEv := monitor.Event{
		Seq:  1,
		At:   time.Now(),
		Type: monitor.EventDelta,
		Updates: []session.SessionState{
			{ID: "sess-1", Activity: session.ActivityIdle},
		},
	}
	if err := srv.HandleEvent(context.Background(), deltaEv); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Both should receive it.
	got1 := readEvent(t, conn1)
	got2 := readEvent(t, conn2)
	if got1.Type != monitor.EventDelta || got2.Type != monitor.EventDelta {
		t.Fatal("both clients should receive delta")
	}
}

func TestServer_MaxConnections(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1"},
	}

	_, ts := newTestServer(t, sessions, wsapi.WithMaxConnections(2))

	// Fill to capacity.
	conn1 := dialWS(t, ts)
	conn2 := dialWS(t, ts)

	// Read snapshots to confirm they connected.
	readEvent(t, conn1)
	readEvent(t, conn2)

	// Third connection should be rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn3, _, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		// Connection might succeed at TCP level but get closed with status.
		_, _, readErr := conn3.Read(ctx)
		if readErr == nil {
			t.Fatal("third connection should have been rejected")
		}
		_ = conn3.CloseNow()
	}
}

func TestServer_SlowClientEviction(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1"},
	}

	srv, ts := newTestServer(t, sessions,
		wsapi.WithSendBuffer(4), // Small buffer for testing.
	)

	conn := dialWS(t, ts)
	// Read snapshot so we know we're connected.
	readEvent(t, conn)

	// Flood events to fill the buffer and trigger eviction.
	for i := 0; i < 20; i++ {
		ev := monitor.Event{
			Seq:  uint64(i + 1),
			At:   time.Now(),
			Type: monitor.EventDelta,
			Updates: []session.SessionState{
				{ID: "sess-1", Activity: session.ActivityWorking},
			},
		}
		_ = srv.HandleEvent(context.Background(), ev)
	}

	// Give eviction a moment to propagate.
	time.Sleep(50 * time.Millisecond)

	if got := srv.ClientCount(); got != 0 {
		t.Fatalf("expected 0 clients after slow-client eviction, got %d", got)
	}
}

func TestServer_LifecycleEvent(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1", Activity: session.ActivityWorking},
	}

	srv, ts := newTestServer(t, sessions)
	conn := dialWS(t, ts)
	readEvent(t, conn) // snapshot

	// Deliver a lifecycle event.
	lcEvent := monitor.Event{
		Seq:  1,
		At:   time.Now(),
		Type: monitor.EventLifecycle,
		Lifecycle: &session.LifecycleEvent{
			Type:      session.EventTerminal,
			SessionID: "sess-1",
			Source:    "test-source",
			From:      session.LifecycleActive,
			To:        session.LifecycleTerminal,
			At:        time.Now(),
		},
	}
	if err := srv.HandleEvent(context.Background(), lcEvent); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := readEvent(t, conn)
	if got.Type != monitor.EventLifecycle {
		t.Fatalf("expected lifecycle event, got %s", got.Type)
	}
	if got.Lifecycle == nil {
		t.Fatal("lifecycle field is nil")
	}
	if got.Lifecycle.SessionID != "sess-1" {
		t.Fatalf("expected sess-1, got %s", got.Lifecycle.SessionID)
	}
}

func TestServer_HealthEvent(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1"},
	}

	srv, ts := newTestServer(t, sessions)
	conn := dialWS(t, ts)
	readEvent(t, conn) // snapshot

	healthEv := monitor.Event{
		Seq:  1,
		At:   time.Now(),
		Type: monitor.EventHealth,
		Health: &monitor.Health{
			Source: "test-source",
			Status: monitor.HealthDegraded,
		},
	}
	if err := srv.HandleEvent(context.Background(), healthEv); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := readEvent(t, conn)
	if got.Type != monitor.EventHealth {
		t.Fatalf("expected health event, got %s", got.Type)
	}
	if got.Health.Status != monitor.HealthDegraded {
		t.Fatalf("expected degraded, got %s", got.Health.Status)
	}
}

func TestServer_RemovedEvent(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1"},
	}

	srv, ts := newTestServer(t, sessions)
	conn := dialWS(t, ts)
	readEvent(t, conn) // snapshot

	removedEv := monitor.Event{
		Seq:     1,
		At:      time.Now(),
		Type:    monitor.EventDelta,
		Removed: []string{"sess-1"},
	}
	if err := srv.HandleEvent(context.Background(), removedEv); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := readEvent(t, conn)
	if got.Type != monitor.EventDelta {
		t.Fatalf("expected delta event, got %s", got.Type)
	}
	if len(got.Removed) != 1 || got.Removed[0] != "sess-1" {
		t.Fatalf("expected removed sess-1, got %v", got.Removed)
	}
}

func TestServer_ConcurrentBroadcast(t *testing.T) {
	sessions := []testSession{
		{ID: "sess-1"},
	}

	srv, ts := newTestServer(t, sessions)
	conn := dialWS(t, ts)
	readEvent(t, conn) // snapshot

	// Fire concurrent events — run with -race to verify safety.
	var wg sync.WaitGroup
	const numEvents = 20
	wg.Add(numEvents)
	for i := 0; i < numEvents; i++ {
		go func(seq int) {
			defer wg.Done()
			ev := monitor.Event{
				Seq:  uint64(seq),
				At:   time.Now(),
				Type: monitor.EventDelta,
				Updates: []session.SessionState{
					{ID: "sess-1", Activity: session.ActivityWorking},
				},
			}
			_ = srv.HandleEvent(context.Background(), ev)
		}(i + 1)
	}
	wg.Wait()

	// Client should have received at least some events.
	count := 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
		count++
		if count >= numEvents {
			break
		}
	}
	if count == 0 {
		t.Fatal("expected at least some events delivered to client")
	}
}

// Ensure we satisfy the EventSink interface at compile time.
var _ monitor.EventSink = (*wsapi.Server)(nil)
