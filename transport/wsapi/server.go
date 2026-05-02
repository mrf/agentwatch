package wsapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mrf/agentwatch/monitor"
)

// Server is a WebSocket transport adapter for agentwatch. It implements
// monitor.EventSink: events from the monitor are fanned out to all
// connected WebSocket clients.
//
// On connect, clients receive an initial snapshot event. Subsequent events
// (deltas, lifecycle, health) are delivered as they arrive from the monitor.
type Server struct {
	mon         *monitor.Monitor
	cfg         serverConfig
	bcast       *broadcaster
	rateLimiter *clientRateLimiter
	logger      *slog.Logger

	mu      sync.Mutex
	stopped bool // guarded by mu; checked in ServeHTTP
}

// NewServer creates a WebSocket server that broadcasts monitor events
// to connected clients. The server implements monitor.EventSink.
func NewServer(mon *monitor.Monitor, opts ...Option) (*Server, error) {
	cfg := defaultServerConfig()
	for _, o := range opts {
		o(&cfg)
	}

	s := &Server{
		mon:         mon,
		cfg:         cfg,
		bcast:       newBroadcaster(cfg.maxConnections, cfg.logger),
		rateLimiter: newClientRateLimiter(cfg.rateLimit, cfg.rateWindow, cfg.rateBurst),
		logger:      cfg.logger,
	}

	return s, nil
}

// HandleEvent implements monitor.EventSink. It fans out the event to all
// connected WebSocket clients. Returns quickly via non-blocking sends.
func (s *Server) HandleEvent(ctx context.Context, ev monitor.Event) error {
	return s.bcast.HandleEvent(ctx, ev)
}

// ServeHTTP handles WebSocket upgrade requests. Attach this to your HTTP
// router at the desired path (e.g., "/ws").
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		http.Error(w, "server stopped", http.StatusServiceUnavailable)
		return
	}

	// Rate limit check.
	if s.rateLimiter != nil {
		decision := s.rateLimiter.Allow(clientAddress(r))
		if !decision.Allowed {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// Origin check.
	if !s.checkOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	// Authentication check.
	if s.cfg.authenticator != nil {
		if _, ok := s.cfg.authenticator.Authenticate(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Accept the WebSocket upgrade.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin is checked above; don't double-check in the library.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Warn("ws upgrade failed", "error", err)
		return
	}

	c := newConn(ws, s.cfg.sendBuffer, s.cfg.writeTimeout)

	// Try to add the connection.
	if !s.bcast.addConn(c) {
		ws.Close(websocket.StatusTryAgainLater, "too many connections")
		return
	}

	s.logger.Info("ws client connected", "addr", r.RemoteAddr)

	// Send initial snapshot.
	s.sendSnapshot(c)

	// Start write pump in background.
	go s.writePump(c)

	// Start heartbeat in background.
	var heartbeatDone chan struct{}
	if s.cfg.heartbeat > 0 {
		heartbeatDone = make(chan struct{})
		go s.heartbeatLoop(c, heartbeatDone)
	}

	// Read pump — blocks until the client disconnects or sends a message.
	s.readPump(r.Context(), c)

	// Cleanup.
	s.bcast.removeConn(c)
	if heartbeatDone != nil {
		<-heartbeatDone
	}
	s.logger.Info("ws client disconnected", "addr", r.RemoteAddr)
}

// Stop disconnects all clients and prevents new connections.
func (s *Server) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.bcast.stop()
}

// ClientCount returns the number of currently connected clients.
func (s *Server) ClientCount() int {
	return s.bcast.clientCount()
}

// sendSnapshot sends a full snapshot event to a single client.
func (s *Server) sendSnapshot(c *conn) {
	sessions := s.mon.Snapshot()
	ev := monitor.Event{
		Type:     monitor.EventSnapshot,
		At:       time.Now(),
		Sessions: sessions,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		s.logger.Warn("snapshot marshal failed", "error", err)
		return
	}
	c.trySend(data)
}

// writePump drains the send channel and writes messages to the WebSocket.
func (s *Server) writePump(c *conn) {
	ctx := context.Background()
	for msg := range c.send {
		writeCtx, cancel := context.WithTimeout(ctx, c.writeTimeout)
		err := c.ws.Write(writeCtx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			s.bcast.removeConn(c)
			return
		}
	}
	// Channel closed — connection is being torn down.
	c.ws.Close(websocket.StatusNormalClosure, "")
}

// readPump reads from the WebSocket until the client disconnects.
// Currently no client->server messages are expected, but we need to
// read to detect disconnection and process control frames.
func (s *Server) readPump(ctx context.Context, c *conn) {
	for {
		_, _, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
	}
}

// heartbeatLoop sends periodic pings to detect dead connections.
func (s *Server) heartbeatLoop(c *conn, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.cfg.heartbeat)
	defer ticker.Stop()
	ctx := context.Background()
	for {
		<-ticker.C
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		pingCtx, cancel := context.WithTimeout(ctx, s.cfg.writeTimeout)
		err := c.ws.Ping(pingCtx)
		cancel()
		if err != nil {
			s.bcast.removeConn(c)
			return
		}
	}
}

// checkOrigin validates the Origin header on a WebSocket upgrade request.
// When allowedOrigins is configured, only those origins are accepted.
// Otherwise, same-origin and localhost are allowed (plan §5 rule 5).
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")

	if len(s.cfg.allowedOrigins) > 0 {
		return s.checkOriginAllowlist(origin)
	}

	// No allowlist — default: same-origin and localhost only.
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	host := parsed.Host
	if host == "" {
		return false
	}

	// Same-origin: origin host matches request host.
	if host == r.Host {
		return true
	}

	// Localhost development.
	switch parsed.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}

	return false
}

// checkOriginAllowlist checks origin against the configured allowlist.
// Matches by exact string or by host component.
func (s *Server) checkOriginAllowlist(origin string) bool {
	if origin == "" {
		return false
	}

	// Fast path: exact match.
	for i := 0; i < len(s.cfg.allowedOrigins); i++ {
		if origin == s.cfg.allowedOrigins[i] {
			return true
		}
	}

	// Slow path: compare host components.
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for i := 0; i < len(s.cfg.allowedOrigins); i++ {
		allowedParsed, err := url.Parse(s.cfg.allowedOrigins[i])
		if err != nil {
			continue
		}
		if parsed.Host == allowedParsed.Host {
			return true
		}
	}

	return false
}
