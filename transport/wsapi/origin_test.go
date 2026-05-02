package wsapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mrf/agentwatch/transport/wsapi"
)

func TestServer_OriginDefault_SameOrigin(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// Same-origin request should succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	origin := "http" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {origin}},
	})
	if err != nil {
		t.Fatalf("same-origin should be allowed: %v", err)
	}
	conn.CloseNow()
}

func TestServer_OriginDefault_Localhost(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// Localhost origin should be allowed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://localhost:3000"}},
	})
	if err != nil {
		t.Fatalf("localhost should be allowed: %v", err)
	}
	conn.CloseNow()
}

func TestServer_OriginDefault_127001(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://127.0.0.1:8080"}},
	})
	if err != nil {
		t.Fatalf("127.0.0.1 should be allowed: %v", err)
	}
	conn.CloseNow()
}

func TestServer_OriginDefault_NoOriginAllowed(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// No origin header should be allowed (e.g., non-browser clients).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("no origin should be allowed: %v", err)
	}
	conn.CloseNow()
}

func TestServer_OriginDefault_RemoteRejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// Remote origin should be rejected when no allowlist is configured.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	_, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://evil.example.com"}},
	})
	if err == nil {
		t.Fatal("remote origin should be rejected")
	}
}

func TestServer_OriginAllowlist_Allowed(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAllowedOrigins([]string{"http://app.example.com"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://app.example.com"}},
	})
	if err != nil {
		t.Fatalf("allowlisted origin should be allowed: %v", err)
	}
	conn.CloseNow()
}

func TestServer_OriginAllowlist_Rejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAllowedOrigins([]string{"http://app.example.com"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	_, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://other.example.com"}},
	})
	if err == nil {
		t.Fatal("non-allowlisted origin should be rejected")
	}
}

func TestServer_OriginAllowlist_NoOriginRejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAllowedOrigins([]string{"http://app.example.com"}),
	)

	// When an allowlist is configured, missing Origin is rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	_, _, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("missing origin should be rejected when allowlist is configured")
	}
}
