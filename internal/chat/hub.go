package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// hubQueueSize is how many commands may wait for the hub. When the queue is
// full, senders wait their turn: memory use stays bounded (JPL rule 3)
// and a burst slows everyone down a little instead of crashing the server.
const hubQueueSize = 1024

// Hub is the heart of the server. It knows which clients are in which room
// and delivers messages to them.
//
// Only the hub's own goroutine (Run) ever reads or writes its maps.
// Everyone else asks it to do things by sending a command on one channel.
// This is Go's motto in action: "Don't communicate by sharing memory;
// share memory by communicating." Because one goroutine owns the maps,
// they need no locks and can't have data races.
//
// The hub never waits for anything slow: no database, no network. Saving
// to the database happens in each client's own goroutine (see client.go),
// and the hub only fans already-saved messages out from memory. One slow
// query can't hold up every room.
type Hub struct {
	commands chan command
	stopped  chan struct{}  // closed when Run returns
	conns    sync.WaitGroup // connections still being served: see Wait
	rooms    map[string]map[*Client]bool
	clients  map[*Client]bool
	now      func() time.Time
}

type commandKind int

const (
	cmdRegister   commandKind = iota // a client connected
	cmdUnregister                    // a client disconnected
	cmdJoin                          // client joins room
	cmdLeave                         // client leaves room
	cmdBroadcast                     // send msg to everyone in room (client is unused)
	cmdDirect                        // send msg to one client
)

// command is one request to the hub. Every kind goes through the same
// channel on purpose: a channel is first-in, first-out, so if a client
// sends "join" and then "send", the hub is guaranteed to process them in
// that order. Separate channels would not guarantee that.
type command struct {
	kind   commandKind
	client *Client
	room   string
	msg    Outbound
}

// closeReason is the WebSocket close code and text a removed client's
// connection is closed with, so the browser knows why it was dropped.
type closeReason struct {
	code websocket.StatusCode
	text string
}

var (
	byeNormal   = closeReason{websocket.StatusNormalClosure, ""}
	byeSlow     = closeReason{websocket.StatusPolicyViolation, "too slow to keep up"}
	byeShutdown = closeReason{websocket.StatusGoingAway, "server shutting down"}
)

// NewHub returns a hub with no clients. Start it with Run.
func NewHub() *Hub {
	return &Hub{
		commands: make(chan command, hubQueueSize),
		stopped:  make(chan struct{}),
		rooms:    make(map[string]map[*Client]bool),
		clients:  make(map[*Client]bool),
		now:      time.Now,
	}
}

// Run is the hub's event loop. Start it once with `go hub.Run(done)`.
// When done is closed it disconnects everyone and returns.
func (h *Hub) Run(done <-chan struct{}) {
	defer close(h.stopped)
	// jpl:loop runs for the life of the server; returns when done is closed.
	for {
		select {
		case cmd := <-h.commands:
			h.apply(cmd)
		case <-done:
			h.shutdown()
			return
		}
	}
}

// Wait blocks until the hub has stopped and every connection it served has
// closed, or until ctx ends.
//
// When Run stops, it only starts the goodbyes: each connection's writeLoop
// then sends a close frame (1001, "going away") and waits for the browser
// to answer it. A server that exited right after Run would cut many of
// them off, and those browsers would see the connection simply drop
// (1006) instead. So main calls Wait before it returns.
func (h *Hub) Wait(ctx context.Context) error {
	finished := make(chan struct{})
	// If ctx ends first, this goroutine stays until the connections have
	// closed, but by then the program is exiting anyway.
	go func() {
		<-h.stopped
		h.conns.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown says goodbye to every client. It doesn't announce "X left" in
// each room: everyone is leaving at once, and with n people in a room
// that would be n*n messages nobody reads.
func (h *Hub) shutdown() {
	for c := range h.clients {
		c.goodbye = byeShutdown
		close(c.send)
	}
	clear(h.clients)
	clear(h.rooms)
}

// submit hands a command to the hub. It waits while the hub's queue is
// full, and returns false if the hub has stopped (server shutdown).
//
// It deliberately doesn't watch the connection's context: a message that
// was already saved must still be broadcast, and an unregister must still
// happen, even if the sender's connection has just died.
func (h *Hub) submit(cmd command) bool {
	// When both cases of a select are ready, Go picks one at random. Check
	// "stopped" on its own first, so a stopped hub reliably says no instead
	// of queueing commands that nobody will ever read.
	select {
	case <-h.stopped:
		return false
	default:
	}
	select {
	case h.commands <- cmd:
		return true
	case <-h.stopped:
		return false
	}
}

// apply carries out one command. Only Run calls it, so it may use the
// hub's maps freely.
func (h *Hub) apply(cmd command) {
	switch cmd.kind {
	case cmdRegister:
		h.clients[cmd.client] = true
	case cmdBroadcast:
		// The message is already saved, so deliver it even if its sender
		// has disconnected in the meantime.
		h.dropSlow(h.broadcast(cmd.room, cmd.msg))
	case cmdUnregister, cmdJoin, cmdLeave, cmdDirect:
		h.applyFromClient(cmd)
	default:
		// Impossible unless someone adds a command kind and forgets it
		// here. Say so loudly, and carry on serving everyone else
		// (JPL rule 5: check for "can't happen", and recover).
		slog.Error("hub: unknown command kind", "kind", int(cmd.kind))
	}
}

// applyFromClient carries out a command that only makes sense while its
// client is still connected.
func (h *Hub) applyFromClient(cmd command) {
	c := cmd.client
	if !h.clients[c] {
		// The client was already removed (for example it was too slow) and
		// its send channel is closed. A command from it can still be queued,
		// so ignore it: sending on a closed channel would crash the server.
		return
	}
	switch cmd.kind {
	case cmdUnregister:
		h.removeClient(c, byeNormal)
	case cmdJoin:
		h.dropSlow(h.join(c, cmd.room))
	case cmdLeave:
		h.dropSlow(h.leave(c, cmd.room))
	case cmdDirect:
		if !c.trySend(cmd.msg) {
			h.removeClient(c, byeSlow)
		}
	case cmdRegister, cmdBroadcast:
		// Handled in apply. They're listed so the exhaustive linter can
		// check that every kind has been thought about.
	}
}

// join adds c to a room and announces it. It returns the clients that were
// too slow to take the announcement.
func (h *Hub) join(c *Client, room string) []*Client {
	members, ok := h.rooms[room]
	if !ok {
		members = make(map[*Client]bool)
		h.rooms[room] = members
	}
	if members[c] {
		return nil // already in the room
	}
	members[c] = true
	c.rooms[room] = true
	return h.broadcast(room, h.systemMessage(c, room, "joined"))
}

// leave takes c out of a room and announces it. It returns the clients that
// were too slow to take the announcement.
func (h *Hub) leave(c *Client, room string) []*Client {
	members := h.rooms[room]
	if !members[c] {
		return nil
	}
	delete(members, c)
	delete(c.rooms, room)
	if len(members) == 0 {
		delete(h.rooms, room) // don't keep empty rooms around forever
		return nil
	}
	return h.broadcast(room, h.systemMessage(c, room, "left"))
}

func (h *Hub) systemMessage(c *Client, room, verb string) Outbound {
	return Outbound{
		Type: TypeSystem, Room: room, UserID: c.user.ID, User: c.user.Username,
		Text: c.user.Username + " " + verb, Time: h.now().UTC(),
	}
}

// broadcast fans a message out to every client in the room. It never
// blocks: a client whose send buffer is full is too slow, and instead of
// waiting for it (which would stall the whole hub) broadcast returns it so
// the caller can drop it.
func (h *Hub) broadcast(room string, msg Outbound) []*Client {
	var slow []*Client
	for c := range h.rooms[room] {
		if !c.trySend(msg) {
			slow = append(slow, c)
		}
	}
	return slow
}

// dropSlow disconnects clients that couldn't keep up.
func (h *Hub) dropSlow(slow []*Client) {
	for _, c := range slow {
		h.removeClient(c, byeSlow)
	}
}

// removal is one client waiting to be removed, and why.
type removal struct {
	client *Client
	why    closeReason
}

// removeClient disconnects c and takes it out of every room.
//
// Leaving a room announces "c left" to the others. That announcement can
// find more clients too slow to take it, who must be removed too, which
// announces more leaves, and so on. Instead of calling itself for those
// (recursion, which JPL rule 1 bans), removeClient keeps a queue of
// clients still to remove.
//
// queued remembers who is already in the queue. Without it, a slow client
// would be added again by every announcement it misses while it waits its
// turn: with n slow clients sharing r rooms that is about r*n*n/2 entries,
// a gigabyte of memory for a few thousand clients. With it, each client
// enters the queue at most once, so the queue never holds more entries
// than there are clients, and the loop ends (JPL rules 2 and 3).
func (h *Hub) removeClient(c *Client, why closeReason) {
	queue := []removal{{c, why}}
	queued := map[*Client]bool{c: true}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		gone := next.client
		if !h.clients[gone] {
			continue // already removed
		}
		if next.why == byeSlow {
			slog.Warn("dropping slow client", "user", gone.user.Username)
		}
		delete(h.clients, gone)
		for room := range gone.rooms {
			for _, slow := range h.leave(gone, room) {
				if !queued[slow] {
					queued[slow] = true
					queue = append(queue, removal{slow, byeSlow})
				}
			}
		}
		// Closing send tells the client's writeLoop to close the
		// connection with this reason. Setting goodbye first is safe
		// without a lock: closing a channel "happens before" the receive
		// that sees it closed.
		gone.goodbye = next.why
		close(gone.send)
	}
}
