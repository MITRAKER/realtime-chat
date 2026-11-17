package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

const (
	sendBuffer      = 64               // messages we queue for a client before calling it "slow"
	writeTimeout    = 10 * time.Second // give up on a write after this long
	pingInterval    = 30 * time.Second // how often we check the connection is alive
	storeTimeout    = 5 * time.Second  // give up on a database call after this long
	maxFrameSize    = 8 << 10          // 8 KB: bigger frames are rejected
	maxRoomName     = 64
	maxTextLength   = 2000
	maxRoomsPerConn = 100 // stops one connection from joining millions of rooms
	historyPageSize = 50
)

// Client is one browser tab connected over a WebSocket.
//
// Each client runs two goroutines:
//   - readLoop: reads frames from the socket, saves messages to the
//     database, and asks the hub to deliver things
//   - writeLoop: takes messages from the send channel and writes them
//
// Splitting them means a slow network write never blocks reading,
// and the hub never touches the network or the database directly.
type Client struct {
	hub   *Hub
	store Store
	conn  *websocket.Conn
	user  auth.User
	send  chan Outbound

	// Two sets of rooms, each owned by exactly one goroutine:
	rooms  map[string]bool // owned by the hub goroutine: who gets broadcasts
	joined map[string]bool // owned by readLoop: may this client post here?
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

// serve registers an authenticated connection with the hub and runs until
// the connection ends.
func (h *Hub) serve(ctx context.Context, conn *websocket.Conn, store Store, user auth.User) {
	c := &Client{
		hub: h, store: store, conn: conn, user: user,
		send:  make(chan Outbound, sendBuffer),
		rooms: map[string]bool{}, joined: map[string]bool{},
	}
	if !h.submit(ctx, command{kind: cmdRegister, client: c}) {
		return
	}

	// connCtx ends when either loop stops, which then stops the other one.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.writeLoop(connCtx, cancel)
	c.readLoop(connCtx)

	// Reading stopped (the browser left or errored). Ask the hub to clean
	// up. This uses the outer ctx on purpose: connCtx may already be
	// cancelled, and select picks randomly between ready cases, so with
	// connCtx the unregister could be skipped and leave a "ghost" member.
	h.submit(ctx, command{kind: cmdUnregister, client: c})
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
				slog.Debug("read error", "user", c.user.Username, "err", err)
			}
			return
		}
		if !c.handle(ctx, in) {
			return
		}
	}
}

// handle acts on one frame from the browser. It runs in the readLoop
// goroutine, so it may wait on the database: that only slows down this
// one client, never the hub or anyone else. It returns false when the
// connection should end.
func (c *Client) handle(ctx context.Context, in Inbound) bool {
	switch in.Type {
	case TypeJoin, TypeLeave, TypeHistory, TypeSend:
	case TypeAuth:
		return c.reply(ctx, Outbound{Type: TypeError, Text: "already authenticated"})
	default:
		return c.reply(ctx, Outbound{Type: TypeError, Text: "unknown message type"})
	}
	room := strings.TrimSpace(in.Room)
	if room == "" || len(room) > maxRoomName {
		return c.reply(ctx, Outbound{Type: TypeError, Text: "room name must be 1-64 characters"})
	}
	switch in.Type {
	case TypeJoin:
		if c.joined[room] {
			return true
		}
		if len(c.joined) >= maxRoomsPerConn {
			return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "too many rooms joined"})
		}
		c.joined[room] = true
		// Subscribe FIRST, then load history. A message sent while the
		// history query runs then reaches us live (and maybe also in the
		// history page; the browser drops duplicates by id). Doing it the
		// other way round would leave a gap where messages are lost.
		if !c.hub.submit(ctx, command{kind: cmdJoin, client: c, room: room}) {
			return false
		}
		return c.sendHistory(ctx, room, 0)

	case TypeLeave:
		if !c.joined[room] {
			return true
		}
		delete(c.joined, room)
		return c.hub.submit(ctx, command{kind: cmdLeave, client: c, room: room})

	case TypeHistory:
		if !c.joined[room] {
			return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "join the room first"})
		}
		return c.sendHistory(ctx, room, in.Before)

	case TypeSend:
		text := strings.TrimSpace(in.Text)
		switch {
		case text == "":
			return true
		case len(text) > maxTextLength:
			return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "message too long"})
		case !c.joined[room]:
			return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "join the room before sending"})
		}
		sctx, cancel := context.WithTimeout(ctx, storeTimeout)
		msg, err := c.store.SaveMessage(sctx, room, c.user, text)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false // shutting down
			}
			slog.Error("save message", "room", room, "err", err)
			return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "could not save message, try again"})
		}
		return c.hub.submit(ctx, command{kind: cmdBroadcast, room: room, msg: msg})
	}
	return true
}

func (c *Client) sendHistory(ctx context.Context, room string, before int64) bool {
	hctx, cancel := context.WithTimeout(ctx, storeTimeout)
	msgs, more, err := c.store.History(hctx, room, before, historyPageSize)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		slog.Error("load history", "room", room, "err", err)
		return c.reply(ctx, Outbound{Type: TypeError, Room: room, Text: "could not load history"})
	}
	return c.reply(ctx, Outbound{Type: TypeHistory, Room: room, Messages: msgs, More: more})
}

// reply sends a message to this client only. It goes through the hub,
// because the hub is the only goroutine allowed to write to c.send.
func (c *Client) reply(ctx context.Context, m Outbound) bool {
	return c.hub.submit(ctx, command{kind: cmdDirect, client: c, msg: m})
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
