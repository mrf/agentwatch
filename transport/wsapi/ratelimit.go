package wsapi

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// maxRateLimitClients bounds the bucket map so distinct client keys cannot
// grow it without limit between idle sweeps.
const maxRateLimitClients = 4096

// rateLimitDecision reports whether a request was allowed and when to retry.
type rateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// clientRateLimiter implements a per-client token bucket rate limiter.
// Ported from agent-racer backend/internal/ws/ratelimit.go.
type clientRateLimiter struct {
	mu         sync.Mutex
	now        func() time.Time
	rate       float64
	burst      float64
	idleTTL    time.Duration
	lastSweep  time.Time
	maxClients int
	clients    map[string]clientTokenBucket
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
		now:        time.Now,
		rate:       float64(limit) / window.Seconds(),
		burst:      float64(burst),
		idleTTL:    2 * window,
		lastSweep:  now,
		maxClients: maxRateLimitClients,
		clients:    make(map[string]clientTokenBucket),
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
		if len(l.clients) >= l.maxClients {
			l.evictLeastRecentlySeen()
		}
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

func (l *clientRateLimiter) evictLeastRecentlySeen() {
	var oldestKey string
	var oldest time.Time
	first := true
	for key, bucket := range l.clients {
		if first || bucket.lastSeen.Before(oldest) {
			oldestKey, oldest, first = key, bucket.lastSeen, false
		}
	}
	delete(l.clients, oldestKey)
}

// parseTrustedProxies parses IP addresses and CIDR prefixes.
func parseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := strings.TrimSpace(entries[i])
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("wsapi: invalid trusted proxy %q: %w", entry, err)
			}
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("wsapi: invalid trusted proxy %q: %w", entry, err)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	addr = addr.Unmap()
	for i := 0; i < len(trusted); i++ {
		if trusted[i].Contains(addr) {
			return true
		}
	}
	return false
}

// clientAddress derives the rate-limit key for a request: the RemoteAddr
// host, unless that host is a trusted proxy, in which case the forwarding
// headers are honored. X-Forwarded-For is walked right to left and the first
// untrusted hop wins; entries left of it were supplied by the client.
func clientAddress(r *http.Request, trusted []netip.Prefix) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	host, _, err := net.SplitHostPort(remote)
	if err != nil || host == "" {
		host = remote
	}
	if host == "" {
		return "unknown"
	}

	remoteAddr, err := netip.ParseAddr(host)
	if err != nil || !isTrusted(remoteAddr, trusted) {
		return host
	}

	if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
		return forwardedClient(forwarded, host, trusted)
	}

	if realIP, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return realIP.Unmap().String()
	}

	return host
}

// forwardedClient walks the X-Forwarded-For hops right to left and returns
// the first untrusted one (or the leftmost). A malformed hop falls back to
// host, the trusted proxy that sent the header.
func forwardedClient(forwarded []string, host string, trusted []netip.Prefix) string {
	hops := strings.Split(strings.Join(forwarded, ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return host
		}
		if !isTrusted(hop, trusted) || i == 0 {
			return hop.Unmap().String()
		}
	}
	return host
}
