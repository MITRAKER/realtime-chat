package chat

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/time/rate"

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
	hub     *Hub
	store   Store
	conn    *websocket.Conn
	user    auth.User
	send    chan Outbound
	limiter *rate.Limiter // frames per second this client may send

	// Two sets of rooms, each owned by exactly one goroutine:
	rooms  map[string]bool // owned by the hub goroutine: who gets broadcasts
	joined map[string]bool // owned by readLoop: may this client post here?

	// goodbye is why the hub removed this client. The hub sets it just
	// before closing send; writeLoop reads it after seeing send closed.
	goodbye closeReason
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
func (h *Hub) serve(ctx context.Context, conn *websocket.Conn, store Store, user auth.User, limiter *rate.Limiter) {
	c := &Client{
		hub: h, store: store, conn: conn, user: user, limiter: limiter,
		send:  make(chan Outbound, sendBuffer),
		rooms: map[string]bool{}, joined: map[string]bool{},
	}
	if !h.submit(command{kind: cmdRegister, client: c}) {
		return
	}

	// connCtx ends when either loop stops, which then stops the other one.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.writeLoop(connCtx, cancel)
	c.readLoop(connCtx)

	// Reading stopped (the browser left or errored). Ask the hub to clean up.
	h.submit(command{kind: cmdUnregister, client: c})
	conn.Close(websocket.StatusNormalClosure, "")
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		var in Inbound
		// A frame that isn't a JSON object of the right shape ends the
		// connection: wsjson.Read itself closes it with code 1007
		// ("invalid frame payload data").
		if err := wsjson.Read(ctx, c.conn, &in); err != nil {
			if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
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
	// Every frame costs one token from the client's bucket. A client that
	// sends faster than the limit gets an error instead of service, so one
	// script can't flood a room (or the database) for everyone else.
	if !c.limiter.Allow() {
		return c.reply(Outbound{Type: TypeError, Text: "too many messages, slow down"})
	}
	switch in.Type {
	case TypeJoin, TypeLeave, TypeHistory, TypeSend:
	case TypeAuth:
		return c.reply(Outbound{Type: TypeError, Text: "already authenticated"})
	default:
		return c.reply(Outbound{Type: TypeError, Text: "unknown message type"})
	}
	room := strings.TrimSpace(in.Room)
	if !validRoomName(room) {
		return c.reply(Outbound{Type: TypeError, Text: "room name must be 1-64 bytes with no control characters"})
	}
	switch in.Type {
	case TypeJoin:
		if c.joined[room] {
			return true
		}
		if len(c.joined) >= maxRoomsPerConn {
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "too many rooms joined"})
		}
		c.joined[room] = true
		// Subscribe FIRST, then load history. A message sent while the
		// history query runs then reaches us live (and maybe also in the
		// history page; the browser drops duplicates by id). Doing it the
		// other way round would leave a gap where messages are lost.
		if !c.hub.submit(command{kind: cmdJoin, client: c, room: room}) {
			return false
		}
		return c.sendHistory(ctx, room, 0)

	case TypeLeave:
		if !c.joined[room] {
			return true
		}
		delete(c.joined, room)
		return c.hub.submit(command{kind: cmdLeave, client: c, room: room})

	case TypeHistory:
		if !c.joined[room] {
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "join the room first"})
		}
		return c.sendHistory(ctx, room, in.Before)

	case TypeSend:
		text := strings.TrimSpace(in.Text)
		switch {
		case text == "":
			return true
		case len(text) > maxTextLength:
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "message too long"})
		case strings.ContainsRune(text, 0):
			// PostgreSQL can't store the NUL character in text, and no
			// human types one, so refuse it before it reaches the database.
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "message contains invalid characters"})
		case !c.joined[room]:
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "join the room before sending"})
		}
		sctx, cancel := context.WithTimeout(ctx, storeTimeout)
		msg, err := c.store.SaveMessage(sctx, room, c.user, text)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false // shutting down
			}
			slog.Error("save message", "room", room, "err", err)
			return c.reply(Outbound{Type: TypeError, Room: room, Text: "could not save message, try again"})
		}
		return c.hub.submit(command{kind: cmdBroadcast, room: room, msg: msg})
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
		return c.reply(Outbound{Type: TypeError, Room: room, Text: "could not load history"})
	}
	return c.reply(Outbound{Type: TypeHistory, Room: room, Messages: msgs, More: more})
}

// validRoomName accepts 1-64 bytes of printable text. Control characters
// (newlines, NUL, terminal escape codes) are refused: they can corrupt logs
// and screens, and PostgreSQL can't store NUL at all.
func validRoomName(room string) bool {
	if room == "" || len(room) > maxRoomName {
		return false
	}
	return !strings.ContainsFunc(room, unicode.IsControl)
}

// reply sends a message to this client only. It goes through the hub,
// because the hub is the only goroutine allowed to write to c.send.
func (c *Client) reply(m Outbound) bool {
	return c.hub.submit(command{kind: cmdDirect, client: c, msg: m})
}

func (c *Client) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// The hub closed our channel: we were removed (too slow,
				// or the server is shutting down). Tell the browser why.
				c.conn.Close(c.goodbye.code, c.goodbye.text)
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
