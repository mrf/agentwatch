package wsapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/sources/mock"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// newTrackedServer returns a server whose channel receives once per
// ServeHTTP return, so tests can observe handler lifetime.
func newTrackedServer(t *testing.T, opts ...wsapi.Option) (*wsapi.Server, *httptest.Server, <-chan struct{}) {
	t.Helper()
	mon, err := monitor.New(monitor.WithSources(mock.New(mock.WithName("test-source"))))
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv, err := wsapi.NewServer(mon, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	returned := make(chan struct{}, 16)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
		returned <- struct{}{}
	}))
	t.Cleanup(func() {
		srv.Stop()
		ts.Close()
	})
	return srv, ts, returned
}

func TestServer_HandlerReturnsPromptlyAfterClientDisconnect(t *testing.T) {
	_, ts, returned := newTrackedServer(t, wsapi.WithHeartbeat(30*time.Second))

	conn := dialWS(t, ts)
	readEvent(t, conn)
	_ = conn.CloseNow()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("ServeHTTP still running 1s after client disconnect; heartbeat loop must stop with the read pump")
	}
}

func TestServer_WriteErrorClosesConnection(t *testing.T) {
	srv, ts, returned := newTrackedServer(t,
		wsapi.WithSendBuffer(1),
		wsapi.WithWriteTimeout(time.Second),
		wsapi.WithHeartbeat(30*time.Second),
	)

	conn := dialWS(t, ts)
	readEvent(t, conn)

	// Stop reading and flood until the write pump's write fails.
	const maxEvents = 1 << 16
	id := strings.Repeat("x", 4096)
	for i := 0; i < maxEvents && srv.ClientCount() != 0; i++ {
		_ = srv.HandleEvent(context.Background(), monitor.Event{
			Seq:     uint64(i + 1),
			At:      time.Now(),
			Type:    monitor.EventDelta,
			Updates: []session.State{{ID: id, Activity: session.ActivityWorking}},
		})
	}
	if n := srv.ClientCount(); n != 0 {
		t.Fatalf("client never evicted, got %d clients", n)
	}

	// The client is still attached and silent; the handler must not wait on it.
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP still blocked after write failure; the server must close the socket")
	}
}
