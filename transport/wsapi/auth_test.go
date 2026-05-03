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

// tokenAuth is a test Authenticator that validates a Bearer token.
type tokenAuth struct {
	token string
}

func (a *tokenAuth) Authenticate(r *http.Request) (wsapi.Identity, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token != a.token {
		return wsapi.Identity{}, false
	}
	return wsapi.Identity{ID: "user-1"}, true
}

func TestServer_Authenticator_Allowed(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAuthenticator(&tokenAuth{token: "secret"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer secret"},
		},
	})
	if err != nil {
		t.Fatalf("authenticated request should succeed: %v", err)
	}
	_ = conn.CloseNow()
}

func TestServer_Authenticator_Rejected(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAuthenticator(&tokenAuth{token: "secret"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	_, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer wrong"},
		},
	})
	if err == nil {
		t.Fatal("unauthenticated request should be rejected")
	}
}

func TestServer_Authenticator_Missing(t *testing.T) {
	sessions := []testSession{{ID: "s1"}}
	_, ts := newTestServer(t, sessions,
		wsapi.WithAuthenticator(&tokenAuth{token: "secret"}),
	)

	// No auth header at all.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	_, _, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("missing auth should be rejected")
	}
}
