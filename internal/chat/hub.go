package chat

import (
	"context"
	"log/slog"
	"time"
)

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

func NewHub() *Hub {
	return &Hub{
		commands: make(chan command, 1024),
		rooms:    make(map[string]map[*Client]bool),
		clients:  make(map[*Client]bool),
		now:      time.Now,
	}
}

// Run is the hub's event loop. Start it once with `go hub.Run(done)`.
// It disconnects everyone and returns when done is closed.
func (h *Hub) Run(done <-chan struct{}) {
	for {
		select {
		case cmd := <-h.commands:
			h.apply(cmd)
		case <-done:
			for c := range h.clients {
				h.removeClient(c)
			}
			return
		}
	}
}

// submit hands a command to the hub. It returns false if ctx ends first
// (for example, the server is shutting down and the hub has stopped).
func (h *Hub) submit(ctx context.Context, cmd command) bool {
	select {
	case h.commands <- cmd:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *Hub) apply(cmd command) {
	c := cmd.client
	switch cmd.kind {
	case cmdRegister:
		h.clients[c] = true
		return
	case cmdBroadcast:
		// The message is already saved, so deliver it even if its sender
		// has disconnected in the meantime.
		h.broadcast(cmd.room, cmd.msg)
		return
	}
	if !h.clients[c] {
		// The client was already removed (for example it was too slow) and
		// its send channel is closed. A command from it can still be queued,
		// so ignore it: sending on a closed channel would crash the server.
		return
	}
	switch cmd.kind {
	case cmdUnregister:
		h.removeClient(c)
	case cmdJoin:
		h.join(c, cmd.room)
	case cmdLeave:
		h.leave(c, cmd.room)
	case cmdDirect:
		if !c.trySend(cmd.msg) {
			h.dropSlow(c)
		}
	}
}

func (h *Hub) join(c *Client, room string) {
	members, ok := h.rooms[room]
	if !ok {
		members = make(map[*Client]bool)
		h.rooms[room] = members
	}
	if members[c] {
		return // already in the room
	}
	members[c] = true
	c.rooms[room] = true
	h.broadcast(room, Outbound{Type: TypeSystem, Room: room, UserID: c.user.ID, User: c.user.Username, Text: c.user.Username + " joined", Time: h.now().UTC()})
}

func (h *Hub) leave(c *Client, room string) {
	members := h.rooms[room]
	if !members[c] {
		return
	}
	delete(members, c)
	delete(c.rooms, room)
	if len(members) == 0 {
		delete(h.rooms, room) // don't keep empty rooms around forever
	}
	h.broadcast(room, Outbound{Type: TypeSystem, Room: room, UserID: c.user.ID, User: c.user.Username, Text: c.user.Username + " left", Time: h.now().UTC()})
}

// broadcast fans a message out to every client in the room.
// It never blocks: a client whose send buffer is full is too slow,
// so we disconnect it instead of letting it stall the whole hub.
func (h *Hub) broadcast(room string, msg Outbound) {
	var slow []*Client
	for c := range h.rooms[room] {
		if !c.trySend(msg) {
			slow = append(slow, c)
		}
	}
	// Remove slow clients after the loop: removing one makes it "leave"
	// its rooms, which changes the map we were looping over.
	for _, c := range slow {
		h.dropSlow(c)
	}
}

func (h *Hub) dropSlow(c *Client) {
	slog.Warn("dropping slow client", "user", c.user.Username)
	h.removeClient(c)
}

func (h *Hub) removeClient(c *Client) {
	if !h.clients[c] {
		return
	}
	delete(h.clients, c)
	for room := range c.rooms {
		h.leave(c, room)
	}
	close(c.send) // tells the client's writer goroutine to shut down
}
