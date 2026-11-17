package chat

import (
	"log/slog"
	"strings"
	"time"
)

const (
	maxRoomName   = 64
	maxTextLength = 2000
	historySize   = 50
)

// Hub is the heart of the server. It owns the map of rooms -> clients.
//
// Only the hub's own goroutine (Run) ever reads or writes that map.
// Everyone else talks to the hub by sending a request on a channel.
// This is Go's motto in action: "Don't communicate by sharing memory;
// share memory by communicating." Because one goroutine owns the map,
// we need no locks around it and can't have data races on it.
type Hub struct {
	store      Store
	register   chan *Client
	unregister chan *Client
	inbound    chan clientMsg
	rooms      map[string]map[*Client]bool
	clients    map[*Client]bool
	now        func() time.Time
}

// clientMsg pairs a message with the client that sent it.
type clientMsg struct {
	from *Client
	msg  Inbound
}

func NewHub(store Store) *Hub {
	return &Hub{
		store:      store,
		register:   make(chan *Client),
		unregister: make(chan *Client),
		inbound:    make(chan clientMsg, 256),
		rooms:      make(map[string]map[*Client]bool),
		clients:    make(map[*Client]bool),
		now:        time.Now,
	}
}

// Run is the hub's event loop. Start it once with `go hub.Run(done)`.
// It returns when done is closed.
func (h *Hub) Run(done <-chan struct{}) {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
		case c := <-h.unregister:
			h.removeClient(c)
		case cm := <-h.inbound:
			h.handle(cm.from, cm.msg)
		case <-done:
			for c := range h.clients {
				h.removeClient(c)
			}
			return
		}
	}
}

func (h *Hub) handle(c *Client, in Inbound) {
	if !h.clients[c] {
		return // client was already removed; its send channel is closed
	}
	room := strings.TrimSpace(in.Room)
	if room == "" || len(room) > maxRoomName {
		c.trySend(Outbound{Type: TypeError, Text: "room name must be 1-64 characters"})
		return
	}
	switch in.Type {
	case TypeJoin:
		h.join(c, room)
	case TypeLeave:
		h.leave(c, room)
	case TypeSend:
		h.send(c, room, in.Text)
	default:
		c.trySend(Outbound{Type: TypeError, Text: "unknown message type: " + in.Type})
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

	// Send the newcomer what they missed, then tell everyone they arrived.
	history, err := h.store.Recent(room, historySize)
	if err != nil {
		slog.Error("load history", "room", room, "err", err)
	}
	c.trySend(Outbound{Type: TypeHistory, Room: room, Messages: history})
	h.broadcast(room, Outbound{Type: TypeSystem, Room: room, User: c.name, Text: c.name + " joined", Time: h.now()})
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
	h.broadcast(room, Outbound{Type: TypeSystem, Room: room, User: c.name, Text: c.name + " left", Time: h.now()})
}

func (h *Hub) send(c *Client, room, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > maxTextLength {
		c.trySend(Outbound{Type: TypeError, Text: "message too long"})
		return
	}
	if !h.rooms[room][c] {
		c.trySend(Outbound{Type: TypeError, Room: room, Text: "join the room before sending"})
		return
	}
	msg, err := h.store.Save(room, c.name, text, h.now())
	if err != nil {
		slog.Error("save message", "room", room, "err", err)
		c.trySend(Outbound{Type: TypeError, Room: room, Text: "could not save message"})
		return
	}
	h.broadcast(room, msg)
}

// broadcast fans a message out to every client in the room.
// It never blocks: a client whose send buffer is full is too slow,
// so we disconnect it instead of letting it stall the whole hub.
func (h *Hub) broadcast(room string, msg Outbound) {
	for c := range h.rooms[room] {
		if !c.trySend(msg) {
			slog.Warn("dropping slow client", "user", c.name)
			h.removeClient(c)
		}
	}
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
