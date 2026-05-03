package wsapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientRateLimiter_Basic(t *testing.T) {
	limiter := newClientRateLimiter(2, time.Minute, 2)
	if limiter == nil {
		t.Fatal("newClientRateLimiter returned nil")
		return
	}

	base := time.Unix(100, 0)
	limiter.now = func() time.Time { return base }

	// First two requests should be allowed (burst=2).
	for i := 0; i < 2; i++ {
		if decision := limiter.Allow("127.0.0.1"); !decision.Allowed {
			t.Fatalf("Allow[%d] rejected unexpectedly", i)
		}
	}

	// Third request should be rate limited.
	decision := limiter.Allow("127.0.0.1")
	if decision.Allowed {
		t.Fatal("third request should be rate limited")
	}
	if decision.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want %v", decision.RetryAfter, 30*time.Second)
	}

	// After token refill, should be allowed again.
	base = base.Add(30 * time.Second)
	if decision := limiter.Allow("127.0.0.1"); !decision.Allowed {
		t.Fatal("request after refill should be allowed")
	}
}

func TestClientRateLimiter_DifferentClients(t *testing.T) {
	limiter := newClientRateLimiter(1, time.Minute, 1)

	base := time.Unix(100, 0)
	limiter.now = func() time.Time { return base }

	// Each client gets its own bucket.
	if d := limiter.Allow("client-a"); !d.Allowed {
		t.Fatal("client-a first request should be allowed")
	}
	if d := limiter.Allow("client-b"); !d.Allowed {
		t.Fatal("client-b first request should be allowed")
	}

	// Both should now be limited.
	if d := limiter.Allow("client-a"); d.Allowed {
		t.Fatal("client-a second request should be limited")
	}
	if d := limiter.Allow("client-b"); d.Allowed {
		t.Fatal("client-b second request should be limited")
	}
}

func TestClientRateLimiter_Nil(t *testing.T) {
	// A nil limiter should always allow.
	var limiter *clientRateLimiter
	if d := limiter.Allow("any"); !d.Allowed {
		t.Fatal("nil limiter should always allow")
	}
}

func TestClientRateLimiter_InvalidParams(t *testing.T) {
	// Invalid parameters should return nil.
	if l := newClientRateLimiter(0, time.Minute, 1); l != nil {
		t.Fatal("expected nil for limit=0")
	}
	if l := newClientRateLimiter(1, 0, 1); l != nil {
		t.Fatal("expected nil for window=0")
	}
	if l := newClientRateLimiter(1, time.Minute, 0); l != nil {
		t.Fatal("expected nil for burst=0")
	}
}

func TestClientRateLimiter_Sweep(t *testing.T) {
	limiter := newClientRateLimiter(1, time.Minute, 1)

	base := time.Unix(100, 0)
	limiter.now = func() time.Time { return base }
	// Align lastSweep with mock clock so sweep timing is predictable.
	limiter.mu.Lock()
	limiter.lastSweep = base
	limiter.mu.Unlock()

	limiter.Allow("stale-client")

	// Advance past idleTTL (2 * window = 2 minutes).
	base = base.Add(3 * time.Minute)

	// Trigger sweep via a new Allow call.
	limiter.Allow("new-client")

	limiter.mu.Lock()
	_, staleExists := limiter.clients["stale-client"]
	limiter.mu.Unlock()
	if staleExists {
		t.Fatal("stale client should have been swept")
	}
}

func TestClientAddress(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		remote   string
		expected string
	}{
		{
			name:     "x-forwarded-for takes first value",
			headers:  map[string]string{"X-Forwarded-For": "198.51.100.7, 203.0.113.9"},
			remote:   "127.0.0.1:1234",
			expected: "198.51.100.7",
		},
		{
			name:     "x-real-ip fallback",
			headers:  map[string]string{"X-Real-IP": "203.0.113.10"},
			remote:   "127.0.0.1:1234",
			expected: "203.0.113.10",
		},
		{
			name:     "remote host without port fallback",
			remote:   "127.0.0.1:1234",
			expected: "127.0.0.1",
		},
		{
			name:     "raw remote address fallback",
			remote:   "unix-socket-client",
			expected: "unix-socket-client",
		},
		{
			name:     "empty everything returns unknown",
			remote:   "",
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ws", nil)
			req.RemoteAddr = tt.remote
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}

			if got := clientAddress(req); got != tt.expected {
				t.Fatalf("clientAddress() = %q, want %q", got, tt.expected)
			}
		})
	}
}
