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
	// held buffers events that arrive between registration and prime, so the
	// snapshot is always the first message on the wire. Bounded by cap(send).
	held   [][]byte
	primed bool
}

func newConn(ws *websocket.Conn, bufSize int, writeTimeout time.Duration) *conn {
	return &conn{
		ws:           ws,
		send:         make(chan []byte, bufSize),
		writeTimeout: writeTimeout,
	}
}

// trySend attempts a non-blocking send on the client's channel.
// Returns true if the message was enqueued (or held until prime), false if
// the buffer is full or the connection is already closed.
func (c *conn) trySend(data []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if !c.primed {
		if len(c.held) >= cap(c.send) {
			return false
		}
		c.held = append(c.held, data)
		return true
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

// prime enqueues the snapshot, then any events held since registration, and
// opens the connection to direct sends. Returns false if the buffer cannot
// hold them or the connection is closed.
func (c *conn) prime(snapshot []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.primed {
		return false
	}
	c.primed = true
	pending := append([][]byte{snapshot}, c.held...)
	c.held = nil
	for i := 0; i < len(pending); i++ {
		select {
		case c.send <- pending[i]:
		default:
			return false
		}
	}
	return true
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
