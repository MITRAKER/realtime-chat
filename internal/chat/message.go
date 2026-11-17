package chat

import "time"

// Message types sent between the browser and the server.
// Every frame on the WebSocket is one JSON object with a "type" field,
// so both sides can switch on it to decide what to do.
const (
	TypeJoin    = "join"    // client -> server: subscribe to a room
	TypeLeave   = "leave"   // client -> server: unsubscribe from a room
	TypeSend    = "send"    // client -> server: post a message to a room
	TypeMessage = "message" // server -> client: a chat message in a room
	TypeHistory = "history" // server -> client: recent messages after a join
	TypeSystem  = "system"  // server -> client: someone joined/left
	TypeError   = "error"   // server -> client: something went wrong
)

// Inbound is what a client sends us.
type Inbound struct {
	Type string `json:"type"`
	Room string `json:"room"`
	Text string `json:"text,omitempty"`
}

// Outbound is what we send to clients.
type Outbound struct {
	Type     string     `json:"type"`
	Room     string     `json:"room,omitempty"`
	ID       int64      `json:"id,omitempty"`
	User     string     `json:"user,omitempty"`
	Text     string     `json:"text,omitempty"`
	Time     time.Time  `json:"time,omitempty"`
	Messages []Outbound `json:"messages,omitempty"`
}
