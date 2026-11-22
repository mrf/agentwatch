package wsapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mrf/agentwatch/internal/hostcheck"
	"github.com/mrf/agentwatch/monitor"
)

// Server is a WebSocket transport adapter for agentwatch. It implements
// monitor.EventSink: events from the monitor are fanned out to all
// connected WebSocket clients.
//
// On connect, clients receive an initial snapshot event as their first
// message. Subsequent events (deltas, lifecycle, health) are delivered as they
// arrive from the monitor.
//
// Seq contract: the snapshot's Seq is the monitor's event sequence number when
// the snapshot was taken (0 if no event has been emitted yet), and the
// snapshot reflects every event with Seq <= its own. Events already in flight
// at connect time may still arrive after the snapshot; clients must drop any
// event whose Seq is <= the snapshot's. There is no resume point: a client
// that is evicted as a slow consumer or disconnects reconnects to receive a
// fresh snapshot.
type Server struct {
	mon         *monitor.Monitor
	cfg         serverConfig
	bcast       *broadcaster
	rateLimiter *clientRateLimiter
	trusted     []netip.Prefix
	logger      *slog.Logger

	mu      sync.Mutex
	stopped bool // guarded by mu; checked in ServeHTTP

	// afterRegister is a test seam; see SetAfterRegisterHook.
	afterRegister func()
}

// NewServer creates a WebSocket server that broadcasts monitor events
// to connected clients. The server implements monitor.EventSink.
func NewServer(mon *monitor.Monitor, opts ...Option) (*Server, error) {
	cfg := defaultServerConfig()
	for _, o := range opts {
		o(&cfg)
	}

	trusted, err := parseTrustedProxies(cfg.trustedProxies)
	if err != nil {
		return nil, err
	}

	s := &Server{
		mon:         mon,
		cfg:         cfg,
		bcast:       newBroadcaster(cfg.maxConnections, cfg.policy, cfg.logger),
		rateLimiter: newClientRateLimiter(cfg.rateLimit, cfg.rateWindow, cfg.rateBurst),
		trusted:     trusted,
		logger:      cfg.logger,
	}

	return s, nil
}

// reject runs the pre-upgrade checks in order and returns the error message
// and status of the first that fails, or a zero status to accept.
func (s *Server) reject(r *http.Request) (string, int) {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return "server stopped", http.StatusServiceUnavailable
	}

	// Host check (DNS rebinding). Before rate limiting so rebound requests,
	// which arrive from loopback, cannot drain the loopback token bucket.
	if !hostcheck.HostAllowed(r.Host, s.cfg.allowedHosts) {
		return "host not allowed", http.StatusForbidden
	}

	if s.rateLimiter != nil && !s.rateLimiter.Allow(clientAddress(r, s.trusted)).Allowed {
		return "rate limit exceeded", http.StatusTooManyRequests
	}

	if !hostcheck.OriginAllowed(r.Header.Get("Origin"), r.Host, s.cfg.allowedOrigins, s.cfg.localhostOriginsAnyPort) {
		return "origin not allowed", http.StatusForbidden
	}

	if s.cfg.authenticator != nil {
		if _, ok := s.cfg.authenticator.Authenticate(r); !ok {
			return "unauthorized", http.StatusUnauthorized
		}
	}
	return "", 0
}

// HandleEvent implements monitor.EventSink. It fans out the event to all
// connected WebSocket clients. Returns quickly via non-blocking sends.
func (s *Server) HandleEvent(ctx context.Context, ev monitor.Event) error {
	return s.bcast.HandleEvent(ctx, ev)
}

// ServeHTTP handles WebSocket upgrade requests. Attach this to your HTTP
// router at the desired path (e.g., "/ws").
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if msg, code := s.reject(r); code != 0 {
		http.Error(w, msg, code)
		return
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
		_ = ws.Close(websocket.StatusTryAgainLater, "too many connections")
		return
	}

	s.logger.Info("ws client connected", "addr", r.RemoteAddr)

	if s.afterRegister != nil {
		s.afterRegister()
	}

	// The client is registered, so every event with a Seq above the snapshot's
	// reaches it, held behind the snapshot by conn.prime.
	if !s.sendSnapshot(c) {
		s.bcast.removeConn(c)
		return
	}

	// Start write pump in background.
	go s.writePump(c) //nolint:contextcheck // lifetime is bound to c.send closing, not the request context

	// Cancelled when the read pump exits so the heartbeat stops with it.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Start heartbeat in background.
	var heartbeatDone chan struct{}
	if s.cfg.heartbeat > 0 {
		heartbeatDone = make(chan struct{})
		go s.heartbeatLoop(ctx, c, heartbeatDone)
	}

	// Read pump — blocks until the client disconnects or sends a message.
	s.readPump(ctx, c)

	// Cleanup.
	cancel()
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

// sendSnapshot builds a snapshot stamped with the monitor's seq and queues it
// as the connection's first message. Returns false if it could not be queued.
func (s *Server) sendSnapshot(c *conn) bool {
	sessions, seq := s.mon.SnapshotWithSeq()
	ev := monitor.Event{
		Seq:      seq,
		Type:     monitor.EventSnapshot,
		At:       time.Now(),
		Sessions: sessions,
	}
	data, err := json.Marshal(applyPolicy(s.cfg.policy, ev))
	if err != nil {
		s.logger.Warn("snapshot marshal failed", "error", err)
		return false
	}
	return c.prime(data)
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
			// A failed write leaves the socket open; close it so readPump returns.
			_ = c.ws.CloseNow()
			return
		}
	}
	// Channel closed — connection is being torn down.
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
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
func (s *Server) heartbeatLoop(ctx context.Context, c *conn, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.cfg.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
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
