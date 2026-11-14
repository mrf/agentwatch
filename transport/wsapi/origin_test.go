package wsapi_test

import (
	"context"
	"net/http"
	"net/url"
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
	_ = conn.CloseNow()
}

func TestServer_OriginDefault_LocalhostSamePort(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// A loopback origin on the server's own port is allowed, even under a
	// different loopback name than the Host.
	_, port := splitTestURL(t, ts.URL)
	conn, _, err := dialWSWith(t, ts.URL, "127.0.0.1:"+port, "http://localhost:"+port)
	if err != nil {
		t.Fatalf("localhost origin on server port should be allowed: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_OriginDefault_127001(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	_, port := splitTestURL(t, ts.URL)
	conn, _, err := dialWSWith(t, ts.URL, "localhost:"+port, "http://127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("127.0.0.1 origin on server port should be allowed: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_OriginDefault_LocalhostOtherPortRejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions)

	// Another local origin (a different dev server or local page) is
	// rejected by default.
	_, resp, err := dialWSWith(t, ts.URL, "", "http://localhost:1")
	if err == nil {
		t.Fatal("localhost origin on another port should be rejected by default")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got resp=%v err=%v", resp, err)
	}
}

func TestServer_LocalhostOriginsAnyPort_Allowed(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions, wsapi.WithLocalhostOriginsAnyPort())

	// Opt-in: a dev server on another port (e.g. Vite on :5173) may connect.
	conn, _, err := dialWSWith(t, ts.URL, "", "http://localhost:5173")
	if err != nil {
		t.Fatalf("localhost origin on another port should be allowed with opt-in: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_LocalhostOriginsAnyPort_RemoteRejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions, wsapi.WithLocalhostOriginsAnyPort())

	_, _, err := dialWSWith(t, ts.URL, "", "http://evil.example.com:5173")
	if err == nil {
		t.Fatal("remote origin should be rejected even with the localhost opt-in")
	}
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
	_ = conn.CloseNow()
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
	_ = conn.CloseNow()
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

// splitTestURL returns the host and port of an httptest server URL.
func splitTestURL(t *testing.T, tsURL string) (host, port string) {
	t.Helper()
	u, err := url.Parse(tsURL)
	if err != nil {
		t.Fatalf("parse %q: %v", tsURL, err)
	}
	return u.Hostname(), u.Port()
}
