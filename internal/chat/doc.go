// Package chat is the real-time part of the server: the hub that knows who
// is in which room, one Client per WebSocket connection, the /ws handler
// that logs connections in, and the JSON protocol they speak.
//
// Read hub.go first (the event loop that owns all shared state), then
// client.go (two goroutines per connection) and handler.go (logging in).
package chat
