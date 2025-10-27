package websocket

import (
	"errors"
	"net/http"
	"sync"
)

// Message type constants compatible with gorilla/websocket.
const (
	TextMessage   = 1
	BinaryMessage = 2
)

// Conn is a lightweight stand-in for a gorilla websocket connection. It only
// stores the last written payload so that unit tests can exercise the code
// paths without requiring a real network dependency.
type Conn struct {
	mu      sync.Mutex
	closed  bool
	payload []byte
}

// WriteMessage records the payload that would have been written to the
// connection. It returns an error when the connection has been marked as
// closed.
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("websocket: connection closed")
	}
	// The stub ignores the messageType and simply stores the payload. The
	// real gorilla/websocket library would write the frame to the network.
	c.payload = append(c.payload[:0], data...)
	return nil
}

// ReadMessage returns the last payload written via WriteMessage. This is
// sufficient for tests that expect a round trip without establishing a real
// websocket connection.
func (c *Conn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return TextMessage, nil, errors.New("websocket: connection closed")
	}
	data := append([]byte(nil), c.payload...)
	return TextMessage, data, nil
}

// Close marks the stub connection as closed.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// Upgrader mimics the gorilla/websocket Upgrader. It simply creates a new stub
// connection without touching the underlying HTTP streams.
type Upgrader struct {
	CheckOrigin func(r *http.Request) bool
}

// Upgrade ignores the HTTP details and returns a fresh stub connection.
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
	if u != nil && u.CheckOrigin != nil {
		if !u.CheckOrigin(r) {
			return nil, errors.New("websocket: origin not allowed")
		}
	}
	return &Conn{}, nil
}
