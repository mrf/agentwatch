package wsapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mrf/agentwatch/transport/wsapi"
)

// upgradeStatus runs a non-upgrade request through ServeHTTP. The rate
// limiter runs before the upgrade, so a limited request returns 429 and an
// allowed one fails later with some other status.
func upgradeStatus(t *testing.T, srv *wsapi.Server, remote, xff string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code
}

func TestServer_RateLimit_SpoofedXFFSharesRemoteBucket(t *testing.T) {
	srv, _ := newTestServer(t, []testSession{{ID: "s1"}},
		wsapi.WithRateLimit(1, 1, time.Minute))

	if code := upgradeStatus(t, srv, "203.0.113.5:1000", "198.51.100.1"); code == http.StatusTooManyRequests {
		t.Fatal("first request should not be rate limited")
	}
	if code := upgradeStatus(t, srv, "203.0.113.5:1001", "198.51.100.2"); code != http.StatusTooManyRequests {
		t.Fatalf("rotated X-Forwarded-For from the same untrusted remote: got %d, want 429", code)
	}
}

func TestServer_RateLimit_TrustedProxyHeaderHonored(t *testing.T) {
	srv, _ := newTestServer(t, []testSession{{ID: "s1"}},
		wsapi.WithRateLimit(1, 1, time.Minute),
		wsapi.WithTrustedProxies([]string{"10.0.0.0/8"}))

	if code := upgradeStatus(t, srv, "10.0.0.1:1000", "198.51.100.1"); code == http.StatusTooManyRequests {
		t.Fatal("first client behind proxy should not be rate limited")
	}
	if code := upgradeStatus(t, srv, "10.0.0.1:1001", "198.51.100.2"); code == http.StatusTooManyRequests {
		t.Fatal("distinct client behind trusted proxy should get its own bucket")
	}
	if code := upgradeStatus(t, srv, "10.0.0.1:1002", "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("repeat client behind trusted proxy: got %d, want 429", code)
	}
}

func TestNewServer_InvalidTrustedProxy(t *testing.T) {
	if _, err := wsapi.NewServer(nil, wsapi.WithTrustedProxies([]string{"bogus"})); err == nil {
		t.Fatal("expected error for invalid trusted proxy entry")
	}
}

func TestServer_DefaultMaxConnections(t *testing.T) {
	const defaultCap = 64

	// Disable rate limiting so every dial comes from loopback unthrottled.
	srv, ts := newTestServer(t, []testSession{{ID: "s1"}}, wsapi.WithRateLimit(0, 0, 0))

	for i := 0; i < defaultCap; i++ {
		readEvent(t, dialWS(t, ts))
	}
	if n := srv.ClientCount(); n != defaultCap {
		t.Fatalf("ClientCount = %d, want %d", n, defaultCap)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
		t.Fatalf("connection past default cap: close status %v (err %v), want StatusTryAgainLater", got, err)
	}
}
