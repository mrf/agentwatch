package wsapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/mrf/agentwatch/monitor"
)

// broadcaster fans out monitor events to connected WebSocket clients.
// It implements monitor.EventSink and returns quickly from HandleEvent
// by using non-blocking sends with slow-client eviction.
type broadcaster struct {
	mu      sync.RWMutex
	clients map[*conn]struct{}

	maxConns int
	logger   *slog.Logger
}

func newBroadcaster(maxConns int, logger *slog.Logger) *broadcaster {
	return &broadcaster{
		clients:  make(map[*conn]struct{}),
		maxConns: maxConns,
		logger:   logger,
	}
}

// HandleEvent implements monitor.EventSink. It serializes the event once
// and fans out to all connected clients via non-blocking channel sends.
// Slow clients (whose buffers are full) are evicted.
func (b *broadcaster) HandleEvent(_ context.Context, ev monitor.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		b.logger.Warn("event marshal failed", "error", err)
		return err
	}

	b.mu.RLock()
	clients := make([]*conn, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.RUnlock()

	for i := 0; i < len(clients); i++ {
		if !clients[i].trySend(data) {
			b.logger.Warn("evicting slow ws client")
			b.removeConn(clients[i])
		}
	}

	return nil
}

// addConn registers a connection. Returns false if the max connection
// limit is reached.
func (b *broadcaster) addConn(c *conn) bool {
	b.mu.Lock()
	if b.maxConns > 0 && len(b.clients) >= b.maxConns {
		b.mu.Unlock()
		return false
	}
	b.clients[c] = struct{}{}
	b.mu.Unlock()
	return true
}

// removeConn deregisters and closes a connection.
func (b *broadcaster) removeConn(c *conn) {
	b.mu.Lock()
	if _, ok := b.clients[c]; ok {
		delete(b.clients, c)
		c.close()
	}
	b.mu.Unlock()
}

// clientCount returns the number of connected clients.
func (b *broadcaster) clientCount() int {
	b.mu.RLock()
	n := len(b.clients)
	b.mu.RUnlock()
	return n
}

// stop disconnects all clients.
func (b *broadcaster) stop() {
	b.mu.Lock()
	clients := make([]*conn, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.clients = make(map[*conn]struct{})
	b.mu.Unlock()

	for i := 0; i < len(clients); i++ {
		clients[i].close()
	}
}
