# realtime-chat

A real-time chat server written in Go, built step by step as a portfolio project
for WebSockets, PostgreSQL, Redis and concurrent programming.

Each step is a pull request, and [LEARNING.md](LEARNING.md) explains how and why
each piece was built.

## Status

| Step | What | Status |
|------|------|--------|
| 1 | WebSocket hub, rooms, message history, concurrency tests | done |
| 2 | Accounts (bcrypt), JWT login, WebSocket auth, PostgreSQL history with paging | done |
| 3 | Professional repo: CI/CD, linters, security scans, NASA/JPL coding rules, releases | next |
| 4 | React + TypeScript frontend, direct messages | planned |
| 5 | Several servers kept in sync with Redis pub/sub, presence, typing indicators, rate limits | planned |
| 6 | Load testing with k6, Docker Compose deployment, architecture write-up | planned |

## Run it

You need [Go 1.26+](https://go.dev/dl/). Docker is optional.

**Quick start, no database** (everything is kept in memory and lost on restart):

```sh
go run ./cmd/server
```

**With PostgreSQL** (accounts and messages are saved):

```sh
docker compose up -d                 # starts PostgreSQL on localhost:5432
cp .env.example .env                 # then put a secret in JWT_SECRET:
#   JWT_SECRET=$(openssl rand -hex 32)
set -a; source .env; set +a
go run ./cmd/server
```

Open http://localhost:8080 in two browser windows, sign up as two different
people, join the same room and chat.

### Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `ADDR` | `:8080` | address to listen on |
| `DATABASE_URL` | empty | PostgreSQL URL; empty means in-memory storage |
| `JWT_SECRET` | random | token signing secret, at least 32 characters |
| `TOKEN_TTL` | `24h` | how long a login lasts |

## API

| Method and path | Body / headers | Response |
|-----------------|----------------|----------|
| `POST /api/register` | `{"username", "password"}` | `201 {"token", "expiresAt", "user"}` |
| `POST /api/login` | `{"username", "password"}` | `200 {"token", "expiresAt", "user"}` |
| `GET /api/me` | `Authorization: Bearer <token>` | `200 {"id", "username"}` |
| `GET /ws` | WebSocket, see below | |
| `GET /healthz` | | `200` while the process runs |
| `GET /readyz` | | `200` when the database answers |

**WebSocket protocol.** Every frame is one JSON object with a `type`:

```text
-> {"type":"auth","token":"<jwt>"}            must be the first frame, within 5 s
<- {"type":"ready","userId":1,"user":"alice"}
-> {"type":"join","room":"general"}
<- {"type":"history","room":"general","messages":[...],"more":true}
-> {"type":"send","room":"general","text":"hi"}
<- {"type":"message","room":"general","id":7,"userId":1,"user":"alice","text":"hi","time":"..."}
-> {"type":"history","room":"general","before":7}   older page
-> {"type":"leave","room":"general"}
<- {"type":"system",...}   someone joined or left
<- {"type":"error","text":"..."}
```

A missing, invalid or expired token closes the socket with code `4401`.
Other close codes: `1001` when the server shuts down (reconnect in a moment),
`1007` for a frame that isn't valid JSON, and `1008` when the server drops a
client that can't keep up.

**Limits.** Each connection may send 10 frames per second on average (bursts
of 20); faster senders get an error frame. Messages are at most 2000 bytes,
room names 1-64 bytes without control characters, and one connection can join
up to 100 rooms. When too many logins arrive at once, `/api/login` and
`/api/register` answer `503` with `Retry-After: 1`.

## Test it

```sh
go test -race ./...
```

Database tests are skipped unless `TEST_DATABASE_URL` is set. With
`docker compose up -d` running:

```sh
TEST_DATABASE_URL='postgres://chat:chat@localhost:5432/chat_test?sslmode=disable' go test -race ./...
```

## Project layout

```text
cmd/server/            main(): configuration, wiring, graceful shutdown
internal/app/          HTTP routes, end-to-end tests
internal/auth/         accounts, password hashing, JWTs, the /api endpoints
internal/chat/         the chat engine: hub, clients, WebSocket handler, protocol
internal/postgres/     PostgreSQL stores and SQL migrations
web/                   the browser test page, embedded into the binary
```
