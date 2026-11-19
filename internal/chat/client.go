package chat

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

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
	maxRoomName     = 64               // bytes
	maxTextLength   = 2000             // bytes, not characters: "é" is 2 bytes
	maxRoomsPerConn = 100              // stops one connection from joining millions of rooms
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
		// The hub has stopped, so the server is shutting down. Say so, as
		// the hub does to everyone it was serving.
		_ = conn.Close(byeShutdown.code, byeShutdown.text)
		return
	}

	// connCtx ends when either loop stops, which then stops the other one.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.writeLoop(connCtx, cancel)
	c.readLoop(connCtx)

	// Reading stopped (the browser left or errored). Ask the hub to clean
	// up. If the hub has already stopped, there's nothing left to clean.
	_ = h.submit(command{kind: cmdUnregister, client: c})
	// The error only says the connection was already closed: nothing to do.
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (c *Client) readLoop(ctx context.Context) {
	// jpl:loop one frame per pass; returns when the connection ends or
	// handle says to stop.
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
		return c.replyError("", "too many messages, slow down")
	}
	room := strings.TrimSpace(in.Room)
	switch in.Type {
	case TypeAuth:
		return c.replyError("", "already authenticated")
	case TypeJoin, TypeLeave, TypeHistory, TypeSend:
		if !validRoomName(room) {
			return c.replyError("", "room name must be 1-64 characters: letters, digits, _ or -")
		}
	default:
		return c.replyError("", "unknown message type")
	}
	switch in.Type {
	case TypeJoin:
		return c.join(ctx, room)
	case TypeLeave:
		return c.leave(room)
	case TypeHistory:
		return c.history(ctx, room, in.Before)
	default: // TypeSend, the only type left
		return c.post(ctx, room, in.Text)
	}
}

// join subscribes the client to a room and sends it the latest messages.
func (c *Client) join(ctx context.Context, room string) bool {
	if c.joined[room] {
		return true
	}
	if len(c.joined) >= maxRoomsPerConn {
		return c.replyError(room, "too many rooms joined")
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
}

func (c *Client) leave(room string) bool {
	if !c.joined[room] {
		return true
	}
	delete(c.joined, room)
	return c.hub.submit(command{kind: cmdLeave, client: c, room: room})
}

// history sends one page of messages older than the id "before".
func (c *Client) history(ctx context.Context, room string, before int64) bool {
	if !c.joined[room] {
		return c.replyError(room, "join the room first")
	}
	return c.sendHistory(ctx, room, before)
}

// post saves a chat message and asks the hub to deliver it.
func (c *Client) post(ctx context.Context, room, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	if problem := textProblem(text); problem != "" {
		return c.replyError(room, problem)
	}
	if !c.joined[room] {
		return c.replyError(room, "join the room before sending")
	}
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	msg, err := c.store.SaveMessage(sctx, room, c.user, text)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false // the connection is closing
		}
		slog.Error("save message", "room", room, "err", err)
		return c.replyError(room, "could not save message, try again")
	}
	return c.hub.submit(command{kind: cmdBroadcast, room: room, msg: msg})
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
		return c.replyError(room, "could not load history")
	}
	return c.reply(Outbound{Type: TypeHistory, Room: room, Messages: msgs, More: more})
}

// roomPattern is the rule for room names: the same characters as
// usernames (ASCII letters, digits, '_' and '-'), 1 to maxRoomName of them.
var roomPattern = regexp.MustCompile(fmt.Sprintf(`^[A-Za-z0-9_-]{1,%d}$`, maxRoomName))

// validRoomName reports whether room follows roomPattern.
//
// Everyone sees room names, so two different names must never look the
// same. That rules out most of Unicode, which is full of characters that
// show nothing at all (U+200B ZERO WIDTH SPACE, U+3164 HANGUL FILLER),
// change nothing visible (U+034F COMBINING GRAPHEME JOINER, variation
// selectors), turn text around (U+202E RIGHT-TO-LEFT OVERRIDE) or copy a
// Latin letter exactly (U+0435, a Cyrillic letter that looks like "e").
// An earlier version refused only what unicode.IsPrint calls unprintable,
// and a review found seven characters that got past it. Telling Unicode
// look-alikes apart is a hard problem (Unicode Technical Standard #39
// describes how), so this rule lists the characters that are allowed
// instead of the ones that are not. It also keeps out control characters,
// which can corrupt logs and terminals, and NUL, which PostgreSQL can't
// store.
func validRoomName(room string) bool {
	return roomPattern.MatchString(room)
}

// textProblem says what is wrong with a chat message, or returns "" if
// nothing is.
func textProblem(text string) string {
	switch {
	case len(text) > maxTextLength:
		return "message too long"
	case strings.ContainsRune(text, 0):
		// PostgreSQL can't store the NUL character in text, and no
		// human types one, so refuse it before it reaches the database.
		return "message contains invalid characters"
	}
	return ""
}

// reply sends a message to this client only. It goes through the hub,
// because the hub is the only goroutine allowed to write to c.send.
func (c *Client) reply(m Outbound) bool {
	return c.hub.submit(command{kind: cmdDirect, client: c, msg: m})
}

func (c *Client) replyError(room, text string) bool {
	return c.reply(Outbound{Type: TypeError, Room: room, Text: text})
}

func (c *Client) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	// jpl:loop returns when the hub closes send, a write or ping fails, or
	// ctx ends.
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// The hub closed our channel: we were removed (too slow,
				// or the server is shutting down). Tell the browser why.
				// An error means the socket is already closed.
				_ = c.conn.Close(c.goodbye.code, c.goodbye.text)
				return
			}
			if !c.write(ctx, msg) {
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

// write sends one message, giving up after writeTimeout.
func (c *Client) write(ctx context.Context, msg Outbound) bool {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(wctx, c.conn, msg) == nil
}
