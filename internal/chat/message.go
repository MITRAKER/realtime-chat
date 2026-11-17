package chat

import "time"

// Message types sent between the browser and the server.
// Every frame on the WebSocket is one JSON object with a "type" field,
// so both sides can switch on it to decide what to do.
const (
	TypeAuth    = "auth"    // client -> server: first frame, carries the login token
	TypeReady   = "ready"   // server -> client: the token was accepted
	TypeJoin    = "join"    // client -> server: subscribe to a room
	TypeLeave   = "leave"   // client -> server: unsubscribe from a room
	TypeSend    = "send"    // client -> server: post a message to a room
	TypeHistory = "history" // both ways: ask for / receive a page of older messages
	TypeMessage = "message" // server -> client: a chat message in a room
	TypeSystem  = "system"  // server -> client: someone joined/left
	TypeError   = "error"   // server -> client: something went wrong
)

// Inbound is what a client sends us.
type Inbound struct {
	Type   string `json:"type"`
	Room   string `json:"room,omitempty"`
	Text   string `json:"text,omitempty"`
	Token  string `json:"token,omitempty"`  // only in "auth"
	Before int64  `json:"before,omitempty"` // only in "history": load messages older than this id
}

// Outbound is what we send to clients.
type Outbound struct {
	Type     string     `json:"type"`
	Room     string     `json:"room,omitempty"`
	ID       int64      `json:"id,omitempty"`
	UserID   int64      `json:"userId,omitempty"`
	User     string     `json:"user,omitempty"`
	Text     string     `json:"text,omitempty"`
	Time     time.Time  `json:"time,omitzero"`
	Messages []Outbound `json:"messages,omitempty"` // only in "history", oldest first
	More     bool       `json:"more,omitempty"`     // only in "history": older messages exist
}
