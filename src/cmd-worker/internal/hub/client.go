package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"artificial.pt/pkg-go-shared/protocol"

	"nhooyr.io/websocket"
)

// MessageHandler is called when a message is received from the hub.
type MessageHandler func(msg protocol.WSMessage)

// Client is a WebSocket client that connects to svc-artificial.
type Client struct {
	serverURL string
	nick      string
	conn      *websocket.Conn
	handler   MessageHandler

	mu             sync.Mutex
	pendingReqs    map[string]chan protocol.WSMessage
	connected      bool
}

// New creates a new hub client.
func New(serverURL, nick string, handler MessageHandler) *Client {
	return &Client{
		serverURL:   serverURL,
		nick:        nick,
		handler:     handler,
		pendingReqs: make(map[string]chan protocol.WSMessage),
	}
}

// connect dials the WebSocket and runs the read loop. Returns when disconnected.
func (c *Client) connect(ctx context.Context) error {
	wsURL := fmt.Sprintf("ws://%s/ws?nick=%s", c.serverURL, c.nick)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	conn.SetReadLimit(-1) // default 32 KiB truncates large task lists / histories
	c.conn = conn
	c.connected = true
	slog.Info("hub connected", "nick", c.nick)

	// readLoop blocks until the connection drops
	c.readLoop(ctx)
	return nil
}

// ConnectWithRetry connects and automatically reconnects on disconnect.
func (c *Client) ConnectWithRetry(ctx context.Context) {
	for {
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return // context cancelled, shutting down
		}
		if err != nil {
			slog.Warn("hub connect failed, retrying in 3s", "err", err)
		} else {
			slog.Info("hub disconnected, reconnecting in 3s")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Client) readLoop(ctx context.Context) {
	defer func() {
		c.connected = false
		if c.conn != nil {
			c.conn.Close(websocket.StatusNormalClosure, "bye")
		}
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("hub disconnected", "err", err)
			}
			return
		}
		var msg protocol.WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Check if this is a response to a pending request.
		if msg.RequestID != "" {
			c.mu.Lock()
			ch, ok := c.pendingReqs[msg.RequestID]
			if ok {
				delete(c.pendingReqs, msg.RequestID)
			}
			c.mu.Unlock()
			if ok {
				ch <- msg
				continue
			}
		}

		// Otherwise, pass to handler in a goroutine so slow handlers
		// (e.g. PushChannelNotification writing to the MCP transport)
		// cannot block the read loop and delay delivery of responses
		// to pending Request() calls.
		if c.handler != nil {
			go c.handler(msg)
		}
	}
}

// Send sends a message to the hub.
func (c *Client) Send(msg protocol.WSMessage) error {
	if !c.connected || c.conn == nil {
		return fmt.Errorf("not connected")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageText, data)
}

// Request sends a message and waits for a response with matching requestId.
func (c *Client) Request(ctx context.Context, msg protocol.WSMessage) (protocol.WSMessage, error) {
	reqID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixMicro()%1000)
	msg.RequestID = reqID

	ch := make(chan protocol.WSMessage, 1)
	c.mu.Lock()
	c.pendingReqs[reqID] = ch
	c.mu.Unlock()

	if err := c.Send(msg); err != nil {
		c.mu.Lock()
		delete(c.pendingReqs, reqID)
		c.mu.Unlock()
		return protocol.WSMessage{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pendingReqs, reqID)
		c.mu.Unlock()
		return protocol.WSMessage{}, ctx.Err()
	case <-time.After(60 * time.Second):
		c.mu.Lock()
		delete(c.pendingReqs, reqID)
		c.mu.Unlock()
		return protocol.WSMessage{}, fmt.Errorf("request timed out")
	}
}

// IsConnected returns whether the client is currently connected.
func (c *Client) IsConnected() bool { return c.connected }
