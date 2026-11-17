# Learning notes

This file grows with every step of the project. Each chapter explains what was
built, why it was built that way, and the ideas you need to build it yourself.
Read the chapter, then read the code it points to, then try the exercise.

---

## Chapter 1: A WebSocket chat server in Go

**Code for this chapter:** `internal/chat/` and `cmd/server/main.go`

### 1.1 Why WebSockets?

Normal HTTP is "ask, then answer": the browser sends a request, the server
replies, and the conversation is over. That is a bad fit for chat, because the
server needs to push a message to you the moment someone else types it. You
never asked for it.

Old workarounds:

- **Polling:** the browser asks "anything new?" every second. Wasteful, and
  messages arrive up to a second late.
- **Long polling:** the server holds the request open until there is news.
  Better, but every message still costs a full HTTP request.

A **WebSocket** starts life as an ordinary HTTP request with a header that says
`Upgrade: websocket`. If the server agrees, the same TCP connection is switched
over to a two-way pipe that stays open. Either side can send a "frame" at any
time. That is exactly what chat needs.

In our code the upgrade happens in `server.go`:

```go
conn, err := websocket.Accept(w, r, nil)
```

After that line, `conn` is a live two-way connection to one browser tab.

> **Security note:** `websocket.Accept` checks the `Origin` header by default and
> refuses connections opened by a different website. Without that, any page you
> visit could open a socket to our server and chat as you. This attack is called
> Cross-Site WebSocket Hijacking.

### 1.2 The message protocol

A WebSocket just carries bytes, so we have to agree on a format. We use one JSON
object per frame, always with a `type` field (`message.go`):

```json
// browser -> server
{"type": "join", "room": "general"}
{"type": "send", "room": "general", "text": "hello"}

// server -> browser
{"type": "history", "room": "general", "messages": [...]}
{"type": "message", "room": "general", "id": 7, "user": "alice", "text": "hello", "time": "..."}
{"type": "system",  "room": "general", "text": "bob joined"}
{"type": "error",   "text": "join the room before sending"}
```

Notice that the sender also receives their own message back from the server.
That is deliberate: it confirms the message was saved, and it means every client
shows messages in the same order (the server's order).

### 1.3 The big idea: one goroutine owns the shared state

The server holds one piece of shared state: which clients are in which room.

```go
rooms map[string]map[*Client]bool   // room name -> set of clients
```

Hundreds of goroutines (one or two per connected browser) want to change this
map at the same time: Alice joins, Bob leaves, Carol sends a message. In Go,
**reading and writing a map from several goroutines at once is a data race**.
The program can crash, or worse, silently corrupt data.

There are two classic fixes:

1. **A mutex (lock).** Every goroutine locks the map, changes it, unlocks it.
2. **A single owner.** Exactly one goroutine is allowed to touch the map.
   Everyone else sends it a request over a **channel**.

We use option 2. It is the pattern behind the Go proverb:

> Don't communicate by sharing memory; share memory by communicating.

`Hub.Run` in `hub.go` is that single owner. It is an endless loop around a
`select`:

```go
for {
    select {
    case c := <-h.register:   // a new browser connected
    case c := <-h.unregister: // a browser disconnected
    case cm := <-h.inbound:   // a browser sent join/leave/send
    case <-done:              // the server is shutting down
    }
}
```

Because only this loop ever touches `rooms`, there is no lock around it and no
way to race on it. The events are also handled strictly one at a time, so the
order of messages in a room is well defined.

Why not a mutex? A mutex works too, and is sometimes faster. The single-owner
design is easier to reason about when one action changes several things at once
(joining updates the room, the client's room list, sends history, and notifies
everyone), and it is easy to explain in an interview.

### 1.4 Two goroutines per connection

Each connected browser is a `Client` (`client.go`) with two goroutines:

```
           browser tab
             |    ^
   readLoop  |    |  writeLoop
             v    |
   hub.inbound    client.send (buffered channel, 64 slots)
             \    ^
              v  /
            Hub.Run
```

- **readLoop** blocks on `wsjson.Read` waiting for the next frame, then passes
  it to the hub through `hub.inbound`.
- **writeLoop** waits for messages on the client's own `send` channel and writes
  them to the socket. Every 30 seconds it also sends a **ping**. If no pong
  comes back, the connection is dead (closed laptop, lost Wi-Fi) and we clean
  up, even if the operating system hasn't noticed yet.

Why split reading and writing? Writing to a slow network can take a while. If
one goroutine did both, a slow write would stop us reading, and the hub would
have to wait on the network. Splitting them keeps the hub fast.

### 1.5 Backpressure: never let one slow client freeze everyone

When the hub broadcasts, it puts the message into each client's `send` channel.
What if one client is on a terrible connection and its channel is full? If the
hub waited, **every** room would freeze because of that one person.

So the hub never waits (`trySend` in `client.go`):

```go
select {
case c.send <- m:  // there's room in the buffer: queued
    return true
default:           // buffer full: don't wait
    return false
}
```

If the buffer is full the hub disconnects that client. Its browser can
reconnect and reload history. Real systems (Slack, Discord) make the same trade:
protect the many from the one. This general idea is called **backpressure**:
deciding what to do when a consumer can't keep up with a producer.

### 1.6 Closing channels safely

When the hub removes a client it calls `close(c.send)`. The writeLoop sees the
channel closed and shuts down. Two rules make this safe:

- Only the **sender** side closes a channel, and the hub is the only sender to
  `c.send`.
- Sending on a closed channel panics. So `handle()` first checks
  `h.clients[c]` and ignores messages from a client that was already removed.
  (A message can still be sitting in `hub.inbound` after its client is gone.)

We found that second rule while writing this chapter. It's exactly the kind of
bug that only appears under real concurrency, which is why the next section
matters.

### 1.7 Testing concurrent code

`hub_test.go` starts a real server with `httptest.NewServer` and connects real
WebSocket clients to it, so the tests exercise the same code the browser does.

The most interesting test is `TestManyConcurrentClients`: 50 clients all send a
message **at the same moment** from 50 goroutines, and every client must receive
all 50 messages.

Run the tests with the **race detector**:

```sh
go test -race ./...
```

`-race` instruments every memory access. If two goroutines touch the same
variable without synchronization, the test fails and prints both stack traces.
Run it on every change. CI (`.github/workflows/ci.yml`) does this on every pull
request.

### 1.8 Graceful shutdown

`cmd/server/main.go` listens for Ctrl+C (`SIGINT`) and `SIGTERM` (what Docker
and Kubernetes send). When one arrives, a context is cancelled, the hub
disconnects everyone cleanly, and `srv.Shutdown` waits up to 10 seconds for
in-flight work. Deploys then don't drop messages halfway through.

### 1.9 Limits that protect the server

Small numbers in the code that matter in production:

| Limit | Where | Why |
|-------|-------|-----|
| 8 KB max frame | `client.go` | a client can't send a 1 GB "message" and eat our memory |
| 2000 chars per message | `hub.go` | same, at the chat level |
| 64-message send buffer | `client.go` | how far behind a client may fall before we drop it |
| 10 s write timeout | `client.go` | a stuck socket can't hold a goroutine forever |
| 50 messages of history | `hub.go` | late joiners catch up without a huge payload |

### 1.10 Try it yourself

1. Run `go run ./cmd/server`, open http://localhost:8080 in two tabs, and chat.
2. Open your browser's DevTools, Network tab, filter by "WS", and click the
   `/ws` request. The "Messages" panel shows every JSON frame going each way.
3. **Exercise:** add a `/rooms` HTTP endpoint that returns how many people are
   in each room. Hint: you can't read `h.rooms` from the HTTP handler (that
   would be a data race). Send the hub a request on a new channel that includes
   a reply channel, and have `Run` answer it. Then run `go test -race`.
4. **Break it on purpose:** in `Hub.broadcast`, replace `trySend` with a plain
   `c.send <- msg`. Can you write a test where one client stops reading and the
   whole room freezes?

### Interview questions this chapter prepares you for

- How is a WebSocket different from HTTP polling?
- How do you avoid data races in Go? When would you pick a channel over a mutex?
- What happens in your server when one client is very slow?
- How do you detect a dead connection?
- How do you test concurrent code?

**Next chapter:** real accounts with hashed passwords and JWT login, and moving
message history into PostgreSQL so it survives a restart.
