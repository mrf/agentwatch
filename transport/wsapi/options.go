package wsapi

import (
	"log/slog"
	"net/http"
	"time"
)

// Option configures a Server.
type Option func(*serverConfig)

// Identity represents an authenticated WebSocket client.
type Identity struct {
	ID string
}

// Authenticator validates WebSocket upgrade requests.
// Implementations should return false to reject the connection.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, bool)
}

type serverConfig struct {
	authenticator  Authenticator
	allowedOrigins []string
	maxConnections int
	heartbeat      time.Duration
	sendBuffer     int
	writeTimeout   time.Duration
	rateLimit      int
	rateBurst      int
	rateWindow     time.Duration
	logger         *slog.Logger
}

func defaultServerConfig() serverConfig {
	return serverConfig{
		heartbeat:    30 * time.Second,
		sendBuffer:   64,
		writeTimeout: 10 * time.Second,
		rateLimit:    12,
		rateBurst:    4,
		rateWindow:   time.Minute,
		logger:       slog.Default(),
	}
}

// WithAuthenticator sets an authenticator for WebSocket upgrade requests.
// When set, connections that fail authentication are rejected.
func WithAuthenticator(a Authenticator) Option {
	return func(c *serverConfig) {
		c.authenticator = a
	}
}

// WithAllowedOrigins sets the allowed Origin header values for WebSocket upgrades.
// When empty (the default), the server allows same-origin and localhost connections.
func WithAllowedOrigins(origins []string) Option {
	return func(c *serverConfig) {
		c.allowedOrigins = origins
	}
}

// WithMaxConnections sets the maximum number of concurrent WebSocket connections.
// Zero means unlimited.
func WithMaxConnections(n int) Option {
	return func(c *serverConfig) {
		c.maxConnections = n
	}
}

// WithHeartbeat sets the interval for WebSocket ping/pong heartbeats.
// Defaults to 30 seconds.
func WithHeartbeat(d time.Duration) Option {
	return func(c *serverConfig) {
		c.heartbeat = d
	}
}

// WithSendBuffer sets the per-client send channel buffer size.
// Defaults to 64. Clients that fall behind by this many messages are evicted.
func WithSendBuffer(n int) Option {
	return func(c *serverConfig) {
		if n > 0 {
			c.sendBuffer = n
		}
	}
}

// WithWriteTimeout sets the maximum duration for a write to complete.
// Defaults to 10 seconds.
func WithWriteTimeout(d time.Duration) Option {
	return func(c *serverConfig) {
		c.writeTimeout = d
	}
}

// WithRateLimit configures connection rate limiting per client IP.
// limit is max connections per window, burst is the initial token count.
// Defaults to 12 per minute with burst of 4.
func WithRateLimit(limit, burst int, window time.Duration) Option {
	return func(c *serverConfig) {
		c.rateLimit = limit
		c.rateBurst = burst
		c.rateWindow = window
	}
}

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *serverConfig) {
		c.logger = l
	}
}
