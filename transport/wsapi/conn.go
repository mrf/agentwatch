package wsapi

import (
	"sync"
	"time"

	"github.com/coder/websocket"
)

// conn wraps a WebSocket connection with a buffered send channel.
// The writePump goroutine drains the channel and writes to the wire.
// Slow clients whose send buffer fills up are evicted by the broadcaster.
type conn struct {
	ws           *websocket.Conn
	send         chan []byte
	writeTimeout time.Duration

	mu     sync.Mutex
	closed bool
}

func newConn(ws *websocket.Conn, bufSize int, writeTimeout time.Duration) *conn {
	return &conn{
		ws:           ws,
		send:         make(chan []byte, bufSize),
		writeTimeout: writeTimeout,
	}
}

// trySend attempts a non-blocking send on the client's channel.
// Returns true if the message was enqueued, false if the buffer is full
// or the connection is already closed.
func (c *conn) trySend(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

// close marks the connection as closed and closes the send channel.
// Idempotent — safe to call multiple times.
func (c *conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.send)
	c.mu.Unlock()
}
