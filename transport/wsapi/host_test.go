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

// dialWSWith dials the test server with an optional Host override and Origin.
func dialWSWith(t *testing.T, tsURL, host, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{Host: host}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": {origin}}
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(tsURL, "http"), opts)
}

func TestServer_Host_DNSRebindingRejected(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}})

	// DNS rebinding: evil.example resolves to 127.0.0.1, so Host and Origin
	// agree and the old same-origin check passed.
	_, resp, err := dialWSWith(t, ts.URL, "evil.example", "http://evil.example")
	if err == nil {
		t.Fatal("rebound Host must be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
}

func TestServer_Host_RebindingWithoutOriginRejected(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}})

	_, resp, err := dialWSWith(t, ts.URL, "evil.example:8080", "")
	if err == nil {
		t.Fatal("unknown Host must be rejected even without Origin")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
}

func TestServer_Host_LocalhostAccepted(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}})

	conn, _, err := dialWSWith(t, ts.URL, "localhost:8080", "http://localhost:8080")
	if err != nil {
		t.Fatalf("localhost Host should be accepted: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_Host_AllowedHostAccepted(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}},
		wsapi.WithAllowedHosts([]string{"agentwatch.lan"}),
	)

	conn, _, err := dialWSWith(t, ts.URL, "agentwatch.lan:8080", "http://agentwatch.lan:8080")
	if err != nil {
		t.Fatalf("configured host should be accepted: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_OriginAllowlist_SchemeMismatchRejected(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}},
		wsapi.WithAllowedOrigins([]string{"https://app.example.com"}),
	)

	_, resp, err := dialWSWith(t, ts.URL, "", "http://app.example.com")
	if err == nil {
		t.Fatal("scheme mismatch must be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
}

func TestServer_OriginAllowlist_PortMismatchRejected(t *testing.T) {
	_, ts := newTestServer(t, []testSession{{ID: "s1"}},
		wsapi.WithAllowedOrigins([]string{"http://app.example.com:3000"}),
	)

	_, _, err := dialWSWith(t, ts.URL, "", "http://app.example.com:3001")
	if err == nil {
		t.Fatal("port mismatch must be rejected")
	}
}
