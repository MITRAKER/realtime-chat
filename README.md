# realtime-chat

A real-time chat server written in Go, built step by step as a portfolio project
for WebSockets, Redis and concurrent programming.

Each step is a pull request, and [LEARNING.md](LEARNING.md) explains how and why
each piece was built.

## Status

| Step | What | Status |
|------|------|--------|
| 1 | WebSocket hub, rooms, message history (in memory), concurrency tests | done |
| 2 | Accounts and login (JWT), PostgreSQL history | planned |
| 3 | React + TypeScript frontend | planned |
| 4 | Several servers kept in sync with Redis pub/sub, presence, typing indicators | planned |
| 5 | Load testing with k6, Docker Compose, architecture write-up | planned |

## Run it

You need [Go 1.24+](https://go.dev/dl/).

```sh
go run ./cmd/server
```

Open http://localhost:8080 in two browser tabs, pick different names and the same
room, and chat. Set `ADDR` to change the port, for example `ADDR=:9000`.

## Test it

```sh
go test -race ./...
```

`-race` turns on Go's race detector, which catches goroutines touching the same
memory without synchronization.

## Project layout

```
cmd/server/       main(): config, HTTP server, graceful shutdown
internal/chat/    the chat engine: hub, clients, message types, storage
web/              the test page, embedded into the binary
```
