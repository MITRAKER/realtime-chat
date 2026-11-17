package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	sendBuffer   = 64               // messages we queue for a client before calling it "slow"
	writeTimeout = 10 * time.Second // give up on a write after this long
	pingInterval = 30 * time.Second // how often we check the connection is alive
	maxFrameSize = 8 << 10          // 8 KB: bigger frames are rejected
)

// Client is one browser tab connected over a WebSocket.
//
// Each client runs two goroutines:
//   - readLoop: reads frames from the socket and hands them to the hub
//   - writeLoop: takes messages from the send channel and writes them
//
// Splitting them means a slow network write never blocks reading,
// and the hub never touches the network directly.
type Client struct {
	hub   *Hub
	conn  *websocket.Conn
	name  string
	send  chan Outbound
	rooms map[string]bool // only touched by the hub goroutine
}

// trySend queues a message without blocking. It returns false if the
// client's buffer is full. Only the hub goroutine calls it.
func (c *Client) trySend(m Outbound) bool {
	select {
	case c.send <- m:
		return true
	default:
		return false
	}
}

// Serve registers the client with the hub and runs until the connection ends.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn, name string) {
	conn.SetReadLimit(maxFrameSize)
	c := &Client{hub: h, conn: conn, name: name, send: make(chan Outbound, sendBuffer), rooms: map[string]bool{}}

	select {
	case h.register <- c:
	case <-ctx.Done():
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.writeLoop(ctx, cancel)
	c.readLoop(ctx)

	// Reading stopped (the browser left or errored). Ask the hub to clean up.
	select {
	case h.unregister <- c:
	case <-ctx.Done():
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		var in Inbound
		if err := wsjson.Read(ctx, c.conn, &in); err != nil {
			var syntaxErr *json.SyntaxError
			if errors.As(err, &syntaxErr) {
				c.conn.Close(websocket.StatusUnsupportedData, "invalid JSON")
			} else if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
				slog.Debug("read error", "user", c.name, "err", err)
			}
			return
		}
		select {
		case c.hub.inbound <- clientMsg{from: c, msg: in}:
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// The hub closed our channel: we were removed (e.g. too slow).
				c.conn.Close(websocket.StatusPolicyViolation, "removed by server")
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(wctx, c.conn, msg)
			wcancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			// A ping that gets no pong means the connection is dead
			// (laptop closed, wifi dropped) even if TCP hasn't noticed yet.
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
