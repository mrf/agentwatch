package wsapi

import (
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimitDecision reports whether a request was allowed and when to retry.
type rateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// clientRateLimiter implements a per-client token bucket rate limiter.
// Ported from agent-racer backend/internal/ws/ratelimit.go.
type clientRateLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	rate      float64
	burst     float64
	idleTTL   time.Duration
	lastSweep time.Time
	clients   map[string]clientTokenBucket
}

type clientTokenBucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

func newClientRateLimiter(limit int, window time.Duration, burst int) *clientRateLimiter {
	if limit <= 0 || window <= 0 || burst <= 0 {
		return nil
	}

	now := time.Now()
	return &clientRateLimiter{
		now:       time.Now,
		rate:      float64(limit) / window.Seconds(),
		burst:     float64(burst),
		idleTTL:   2 * window,
		lastSweep: now,
		clients:   make(map[string]clientTokenBucket),
	}
}

func (l *clientRateLimiter) Allow(clientID string) rateLimitDecision {
	if l == nil {
		return rateLimitDecision{Allowed: true}
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	bucket, ok := l.clients[clientID]
	if !ok {
		bucket = clientTokenBucket{
			tokens:   l.burst,
			last:     now,
			lastSeen: now,
		}
	}

	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(l.burst, bucket.tokens+(elapsed*l.rate))
		bucket.last = now
	}

	bucket.lastSeen = now

	if bucket.tokens >= 1 {
		bucket.tokens--
		l.clients[clientID] = bucket
		return rateLimitDecision{Allowed: true}
	}

	l.clients[clientID] = bucket

	// rate is always positive here — the constructor returns nil for non-positive inputs.
	waitSeconds := (1 - bucket.tokens) / l.rate

	return rateLimitDecision{
		Allowed:    false,
		RetryAfter: time.Duration(math.Ceil(waitSeconds * float64(time.Second))),
	}
}

func (l *clientRateLimiter) sweep(now time.Time) {
	if len(l.clients) == 0 || now.Sub(l.lastSweep) < l.idleTTL {
		return
	}

	for key, bucket := range l.clients {
		if now.Sub(bucket.lastSeen) >= l.idleTTL {
			delete(l.clients, key)
		}
	}

	l.lastSweep = now
}

// clientAddress extracts the client IP from an HTTP request.
func clientAddress(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		// X-Forwarded-For: client, proxy1, proxy2 — take the leftmost.
		ip := strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
		if ip != "" {
			return ip
		}
	}

	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}

	if trimmed := strings.TrimSpace(r.RemoteAddr); trimmed != "" {
		return trimmed
	}

	return "unknown"
}
