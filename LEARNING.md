# Learning notes

This file grows with every step of the project. Each chapter explains what was
built, why it was built that way, and the ideas you need to build it yourself.
Read the chapter, then read the code it points to, then try the exercise.

---

## Chapter 1: A WebSocket chat server in Go

**Code for this chapter:** `internal/chat/` and `cmd/server/main.go`

> Chapter 2 changes some of this code: the hub stops calling the store,
> and clients log in. The ideas in this chapter still apply. Section 2.8
> explains what moved and why.

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
and Kubernetes send). When one arrives, a context is cancelled, the hub closes
every chat connection with code 1001 ("going away", so browsers know to
reconnect), and `srv.Shutdown` stops accepting new connections and waits up to
10 seconds for HTTP requests already running. Section 2.13 explains a bug the
first version of this had.

### 1.9 Limits that protect the server

Small numbers in the code that matter in production:

| Limit | Where | Why |
|-------|-------|-----|
| 8 KB max frame | `client.go` | a client can't send a 1 GB "message" and eat our memory |
| 2000 bytes per message | `client.go` | same, at the chat level (bytes, not characters: `é` is 2 bytes) |
| 64-message send buffer | `client.go` | how far behind a client may fall before we drop it |
| 10 s write timeout | `client.go` | a stuck socket can't hold a goroutine forever |
| 50 messages of history | `client.go` | late joiners catch up without a huge payload |

Chapter 2 adds more: a rate limit per connection, room-name rules, and HTTP
server timeouts (section 2.13).

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

---

## Chapter 2: Accounts, login tokens and PostgreSQL

**Code for this chapter:** `internal/auth/`, `internal/postgres/`,
`internal/chat/handler.go`, `internal/chat/client.go`, `internal/app/`

In chapter 1 anyone could type any name. Now people sign up with a password,
log in, and the chat server knows who they really are. Messages are saved in
PostgreSQL, so they survive a restart.

### 2.1 Authentication vs. authorization

Two words that sound alike and mean different things:

- **Authentication** answers "who are you?" (logging in).
- **Authorization** answers "what are you allowed to do?" (for example, you
  may only post in rooms you joined).

This chapter is mostly about authentication. The "join before you send" rule
from chapter 1 is a small piece of authorization.

### 2.2 Never store passwords, store hashes

If the database leaks (it happens to big companies every year), the attacker
must not get anyone's password. So we never store the password itself.

A **hash function** turns any input into a fixed-size fingerprint, and it
can't be reversed. At login we hash what the user typed and compare
fingerprints. But ordinary hashes like SHA-256 are a bad choice for passwords,
because they are *fast*: a graphics card can try billions of guesses per
second against a leaked hash.

We use **bcrypt** (`internal/auth/password.go`), which is built for passwords:

- **It's deliberately slow.** The *cost* setting (we use 12) means about a
  quarter of a second per hash. A user logging in doesn't notice. An attacker
  trying billions of guesses is slowed down by a factor of millions.
- **It adds a random salt.** Two users with the password `sunshine123` get
  completely different hashes, so an attacker can't crack everyone at once or
  use precomputed tables. The salt is stored inside the hash string, so we
  don't manage it ourselves.
- **It has a 72-byte limit.** bcrypt ignores anything after 72 bytes. Some
  libraries silently cut the password, which can surprise people with long
  passphrases. We reject passwords over 72 bytes instead (`ValidatePassword`).

> The newer algorithm **Argon2id** is the current first recommendation (it
> also uses lots of memory, which hurts graphics-card attacks). bcrypt is still
> considered acceptable, is everywhere, and is simpler to start with.

Try it: run the tests in `auth_test.go`. `TestPasswordIsHashedWithSalt`
hashes the same password twice and checks the results differ.

### 2.3 Logging in without leaking information

Look at `Service.Login` in `internal/auth/service.go`. Two details matter:

1. **One error for everything.** Wrong username and wrong password both return
   `invalid username or password`. If we said "no such user", attackers could
   use the login form to discover who has an account. This is called
   **username enumeration**.
2. **Same speed for everything.** Even with one message, *time* can leak the
   answer: an unknown user fails in 1 ms, but a wrong password takes 250 ms
   because bcrypt runs. So for unknown users we still run bcrypt against a dummy
   hash. Both cases now take the same time. This is a **timing attack**
   defence.

Usernames are compared **case-insensitively**: `Alice` and `alice` are the same
account, so nobody can register `ALICE` while `alice` exists. Usernames may
only use ASCII letters, digits, `_` and `-`, which also rules out Unicode
look-alikes such as `аlice` spelled with a Cyrillic `а`. Some look-alikes are
still possible, for example `aIice` with a capital i. Large sites handle those
with fonts that tell the letters apart, or by blocking confusable names.

### 2.4 Tokens: proving who you are on every request

HTTP is stateless: every request stands alone. After logging in, the browser
needs something to show on each later request that says "I'm Alice". There are
two common designs:

| | Server sessions | Signed tokens (JWT) |
|---|---|---|
| What the browser holds | a random session ID | a token containing the user's ID, signed |
| How the server checks it | look the ID up in a database or Redis | check the signature with a secret key |
| Logging someone out instantly | easy: delete the session | hard: the token stays valid until it expires |
| Several servers | all need the session store | any server with the key can check it |

We use **JWTs** (JSON Web Tokens), because in step 5 we run several chat
servers and every one of them must be able to check a token without a shared
lookup. The trade-off, that you can't revoke a token early, is handled by
giving tokens a limited lifetime (24 hours by default).

A JWT looks like `xxxxx.yyyyy.zzzzz`: three base64 parts.

```
header    {"alg": "HS256", "typ": "JWT"}
payload   {"sub": "42", "name": "alice", "iss": "realtime-chat", "exp": 1763425000, "iat": 1763338600}
signature HMAC-SHA256(header + "." + payload, secret)
```

The payload is only **encoded, not encrypted**. Paste any token into
jwt.io and you can read it. So never put secrets in it. What makes it
trustworthy is the **signature**: only someone who knows the server's secret
can produce a valid one, so if anyone changes `"sub": "42"` to `"sub": "1"`, the
signature no longer matches. `TestBadTokensAreRejected` tries exactly that.

Things `Tokens.Parse` (`internal/auth/tokens.go`) checks, and why:

- **The algorithm is pinned to HS256.** Early JWT libraries trusted the `alg`
  field in the token itself. Attackers sent tokens with `"alg": "none"` (no
  signature at all) and were let in. `jwt.WithValidMethods` closes that hole,
  and the test suite includes an `alg: none` token to prove it.
- **The token must have an expiry, and it must be in the future.**
- **The issuer must be us** (`iss`), so tokens made for another app with the
  same key don't work here.
- **The secret must be at least 32 bytes.** HS256 uses a 256-bit key; a short
  secret like `password` can be brute-forced offline from any one token.

The secret comes from the `JWT_SECRET` environment variable. It must never be
in the code or in git. Section 2.12 covers configuration.

### 2.5 Logging in over a WebSocket

For normal HTTP calls the browser sends the token in a header:

```
GET /api/me
Authorization: Bearer eyJhbGciOi...
```

Here's the catch: **the browser's WebSocket API can't set custom headers.**
Common workarounds:

| Option | Problem |
|---|---|
| Token in the URL: `/ws?token=...` | URLs end up in server logs, proxy logs and browser history |
| Cookie | works, but cookies are sent automatically, so you need protection against other sites using them (CSRF) |
| Abuse the `Sec-WebSocket-Protocol` header | a hack, and it confuses proxies |
| **First message is the token** | the connection is briefly unauthenticated |

We use the last one (`internal/chat/handler.go`):

```
browser: opens /ws
browser: {"type": "auth", "token": "eyJ..."}
server:  {"type": "ready", "userId": 42, "user": "alice"}
```

To limit the downside, an unauthenticated connection gets **5 seconds** to
send a valid token and can do nothing else. Otherwise a timer closes it with
close code **4401**. WebSocket close codes 4000-4999 are free for apps to
use, and 4401 mirrors HTTP's "401 Unauthorized". The test page shows "Your
login expired" when it sees that code.

One more detail: a token is only valid until it expires, but a WebSocket can
stay open for days. So the handler sets a timer (`time.AfterFunc`) that closes
the connection the moment the token expires. `TestConnectionClosesWhenTokenExpires`
checks it with a 2-second token.

### 2.6 The database schema

`internal/postgres/migrations/0001_users_and_messages.sql` creates two tables:

```sql
users    (id, username, password_hash, created_at)
messages (id, room, user_id -> users.id, text, created_at)
```

Things worth noticing:

- **`GENERATED ALWAYS AS IDENTITY`** gives every row a new, increasing number.
  We use the message id as the order of messages, so the browser can sort by it.
- **`REFERENCES users (id)`** is a *foreign key*: Postgres refuses a message
  from a user that doesn't exist (`TestMessagesNeedARealUser`).
- **`CREATE UNIQUE INDEX ... ON users (lower(username))`** enforces
  case-insensitive unique names *inside the database*. The Go code doesn't check
  "is this name free?" before inserting, because two sign-ups at the same
  instant could both see "free". Instead it just inserts and translates the
  database's "duplicate" error (code `23505`) into `ErrUsernameTaken`.
  `ConcurrentSignupsForSameName` fires 10 sign-ups at once and expects exactly one
  winner. Letting the database enforce the rule is the only reliable way under
  concurrency.
- **`CREATE INDEX ... ON messages (room, id)`** makes "the latest 50 messages in
  room X" fast. An index is like a book's index: Postgres jumps straight to the
  room's rows, already sorted by id, instead of reading the whole table.
  `TestHistoryUsesTheIndex` inserts 20,000 rows and checks with `EXPLAIN` that
  Postgres really uses it.

**SQL injection.** Every query passes values separately from the SQL text:

```go
pool.QueryRow(ctx, `SELECT ... WHERE lower(username) = lower($1)`, username)
```

The database receives the SQL and the values separately, so a username like
`x'); DROP TABLE users; --` is just an odd name (`TestSQLInjectionIsJustText`).
Never build SQL by gluing strings together.

### 2.7 Migrations: changing the schema safely

A **migration** is a numbered SQL file that changes the schema. Rules:

- Files run in name order, once each. The table `schema_migrations` records
  which ones already ran.
- Each file runs in a **transaction**, so a failing migration leaves no
  half-made tables behind.
- Once a migration is merged, never edit it. Add a new file instead, because
  other databases have already run the old version.
- The files are compiled into the binary with `go:embed`, so the code and its
  schema always ship together.

In step 5 we start three servers at the same time, and all three will try to
migrate. `Migrate` takes a Postgres **advisory lock** first: a named lock that
only one connection can hold. The other servers wait, then find nothing left
to do. `TestMigrateIsIdempotentAndSafeInParallel` runs five migrations at once.

### 2.8 Don't block the event loop

In chapter 1 the hub saved messages itself, which was fine for an in-memory
map. A database call takes about a millisecond over the network. If the hub
waited on it, **every room on the server** would wait too: the hub could
handle at most about 1,000 messages per second, and one slow query would freeze
everyone.

The fix is a rule you'll meet everywhere (Node.js, browsers, game engines):
**the event loop must never wait on I/O.** So the work moved:

```
before:  readLoop -> hub: "save and broadcast this"     (hub waits on the DB)
after:   readLoop saves the message itself (only this client waits)
         readLoop -> hub: "broadcast this saved message" (hub stays in memory)
```

Each client's `readLoop` can wait on the database as long as it likes; that
only slows that one person. The **connection pool** (`internal/postgres/postgres.go`)
caps how many queries run at once. If the database falls behind, readers
queue up for a connection, stop reading from their sockets, and TCP slows the
senders down. This is backpressure again, this time end to end.

Because readLoop can't write to `c.send` (only the hub may, see 1.6), replies
like errors and history pages go through the hub as "direct" commands.

There's also a subtle detail about **ordering**. Every command, from join to
broadcast, goes through **one** channel. Channels are first-in, first-out, so
when a client sends "join" and then "send", the hub sees them in that order.
With separate channels for each kind, Go's `select` picks randomly among
ready channels and could handle the broadcast before the join.

### 2.9 Joining without missing messages

When you join a room you need two things: the recent history, and every new
message from then on. Done naively there is a gap:

```
1. load history     (sees messages up to #10)
2. message #11 is sent and broadcast   <- you're not subscribed yet: lost!
3. subscribe
```

We do it the other way round (`case TypeJoin` in `client.go`):

1. **Subscribe first** (send `join` to the hub).
2. **Then load history.**
3. The browser **drops duplicates by message id**.

Why can't a message be lost now? Take any message M:

- If M was saved before our history query started, the query returns it.
- If M was saved after our query started, its broadcast command was sent to
  the hub after our join command (we sent join *before* querying). The
  channel is first-in, first-out, so the hub adds us to the room before it
  delivers M, and we get M live.

A message saved during the query may show up in both places, which is why the
browser keys messages by id. "Subscribe, then snapshot, then de-duplicate" is
a standard pattern. You'll see it again with Redis in step 5.

Ordering on screen: two people can send at the same moment and their
broadcasts can arrive in either order, so the browser always **sorts by id**.
The database decides the order, not the network.

### 2.10 Paging through history: keyset vs. OFFSET

The obvious way to page is `LIMIT 50 OFFSET 100`. It has two problems:

- **It gets slower the further back you go.** Postgres reads and throws
  away the first 100 rows every time. OFFSET 100000 reads 100,000 rows.
- **Pages shift.** If 3 new messages arrive while you're scrolling, every page
  moves by 3 and you see duplicates.

**Keyset pagination** asks "the 50 messages *before id 1234*":

```sql
SELECT ... FROM messages WHERE room = $1 AND id < $2 ORDER BY id DESC LIMIT 51
```

With the `(room, id)` index this is equally fast for page 1 and page 10,000,
and new messages don't move anything. We ask for **51** rows when we need 50:
if the 51st exists, there are older messages, and the reply says `"more": true`.
That's how the "Load older messages" button knows whether to appear, without a
separate `COUNT(*)` query.

### 2.11 Testing with a real database

Mocks (fake databases) are fast but can lie: they don't enforce unique indexes,
foreign keys, or Postgres's exact behaviour. So:

- **Contract tests** (`internal/chat/storetest`, `internal/auth/authtest`)
  describe how *any* store must behave. The same tests run against the
  in-memory store and against Postgres. If they both pass, the in-memory store
  is a trustworthy stand-in for other tests.
- **Each test gets its own schema** (`internal/postgres/postgrestest`): a fresh,
  empty namespace inside the database that is dropped afterwards. Tests can't
  see each other's rows, so they can run in parallel.
- **No database? Tests skip.** Without `TEST_DATABASE_URL`, `go test ./...`
  still passes, skipping only the database tests. CI always sets it (a
  Postgres "service container" runs next to the tests).
- **One end-to-end test** (`internal/app/app_test.go`) signs up through the
  real HTTP API, chats over real WebSockets, then starts a *new* server on
  the same database and checks that the login and the history are still
  there.

### 2.12 Configuration and secrets

The server reads its settings from **environment variables** (`cmd/server/main.go`):
`ADDR`, `DATABASE_URL`, `JWT_SECRET`, `TOKEN_TTL`. The same binary runs
on your laptop, in CI and in production, and secrets never appear in the code
or in git. `.env.example` lists them; copy it to `.env` (which `.gitignore`
keeps out of git).

Friendly defaults keep learning easy: with no `DATABASE_URL` everything is
kept in memory, and with no `JWT_SECRET` a random one is generated. The server
logs a warning in both cases, because those defaults would be wrong in
production.

### 2.13 Hardening: what a security review found

After building this step I reviewed it the way an attacker would: by trying to
break it. Each problem below was first reproduced with a failing test, then
fixed. They are classics, and you will meet them again in every server you
build.

**1. Slowloris: connections that never finish.** Go's `http.Server` has no
timeouts unless you set them. A client can open a connection and send one
header byte every few seconds, and the server waits forever. A few thousand of
those use up the server's connections and memory. This attack is called
**Slowloris**. `app.NewHTTPServer` now sets four timeouts:

| Timeout | Default | What it limits |
|---|---|---|
| `ReadHeaderTimeout` | 5 s | time to send the request headers |
| `ReadTimeout` | 15 s | time to send the whole request, body included |
| `WriteTimeout` | 15 s | time to send the response |
| `IdleTimeout` | 60 s | how long a kept-alive connection may sit unused |

Do these break WebSockets, which stay open for hours? No. When a request is
upgraded, Go's server hands the raw connection over (it's "hijacked") and
clears these deadlines. The chat code has its own timeouts (pings, write
timeouts). `TestServerTimeoutsDoNotCutWebSockets` proves it, and
`TestSlowlorisClientIsDisconnected` plays the attacker.

**2. Login floods: bcrypt as a weapon.** bcrypt is slow on purpose: one check
keeps a CPU core busy for about 250 ms. That's what makes stolen hashes hard to
crack, but it also means about 16 login attempts per second, even with wrong
passwords, keep 4 cores at 100% and freeze the chat for everyone. The fix in
`internal/auth/service.go` is a **semaphore**: a buffered channel with one slot
per hash allowed to run at once (half the CPU cores). Sending takes a slot,
receiving gives it back:

```go
select {
case s.hashSlots <- struct{}{}: // got a slot
case <-wait.C:
	return ErrBusy // no slot within 3 s: ask the browser to retry
case <-ctx.Done():
	return ctx.Err() // the browser gave up waiting
}
defer func() { <-s.hashSlots }()
```

When the slots stay busy, the server answers **503 Service Unavailable** with a
`Retry-After: 1` header instead of queueing forever. This is called **load
shedding**: refusing some requests quickly is better than answering all of them
too late. Real products also limit login attempts per IP address and per
account; step 5 adds that with Redis.

**3. One client flooding a room.** A script can send thousands of frames a
second, and each message is written to the database and copied to everyone in
the room. Every connection now has a **token bucket** (`golang.org/x/time/rate`):
the bucket holds up to 20 tokens and refills at 10 per second, and each frame
costs one token. A short burst is fine; a sustained flood gets
`too many messages, slow down`. Each connection has its own bucket, so one
abuser can't use up anyone else's allowance (`TestFloodingClientIsSlowedDown`).
Someone could still open many connections; per-user limits come in step 5.

**4. Input that isn't really text.** PostgreSQL can't store the NUL character
(`\x00`) in a `text` column and returns an error instead. So a login with a NUL
in the username caused a 500 Internal Server Error, and a chat message with one
failed to save. Room names also accepted newlines and terminal escape codes,
which can fake extra lines in log files or recolour an admin's terminal. Now:

- `Login` checks the username format before touching the database. A name that
  could never have been registered fails like any wrong login (and still takes
  bcrypt time, so it leaks nothing).
- Room names must be 1-64 bytes with no control characters (`unicode.IsControl`).
- Messages containing NUL are refused.

The lesson: validate at the edge, as data comes in, and write down exactly what
"valid" means. Notice too that the message limit is 2000 **bytes**, not
characters: `len()` of a Go string counts bytes, and `é` takes two.

**5. A saved message that never arrived.** After saving a message, the reader
goroutine handed it to the hub like this:

```go
select {
case h.commands <- cmd:
case <-ctx.Done(): // the sender's connection has ended
}
```

If the sender closed the tab right after sending, `ctx` was already done. When
several cases of a `select` are ready, **Go picks one at random**. So sometimes
the broadcast was skipped: the message was in the database (people saw it later
in the history) but never arrived live. The same pattern could skip the
"unregister" command and leave a ghost client in the hub. Now `submit` waits
only for the hub, and gives up only when the hub itself has stopped. Whenever
two cases of a `select` can be ready at once, ask: "what if Go picks the other
one?"

**6. Saying why a connection closed.** WebSocket close codes tell the browser
why it was disconnected, so it can react properly:

| Code | Meaning in this app | What the browser should do |
|---|---|---|
| 1000 | normal close | nothing |
| 1001 | server is shutting down | reconnect in a moment |
| 1007 | a frame that isn't valid JSON of the right shape | fix the client code |
| 1008 | removed for breaking a rule (too slow to keep up) | show the reason |
| 4401 | missing, invalid or expired token | log in again |

Two things were wrong. A client that never sent its token was cut off without
any code (the read deadline killed the socket); now a timer closes it with
4401. And on shutdown the server cancelled every request in progress the moment
you pressed Ctrl+C, so a login running at that instant failed. Now `Shutdown`
lets running requests finish, while the hub closes every chat connection with
1001.

The hub records *why* it removed a client (`closeReason` in `hub.go`) just
before closing that client's `send` channel, and the client's writer goroutine
reads it after seeing the channel closed. That's safe without a lock because of
a rule in Go's memory model: **closing a channel happens before a receive that
sees it closed.** Everything the hub wrote before `close` is visible to the
goroutine that sees the close.

### 2.14 Try it yourself

1. Start Postgres and the server:

   ```sh
   docker compose up -d
   export DATABASE_URL='postgres://chat:chat@localhost:5432/chat?sslmode=disable'
   export JWT_SECRET=$(openssl rand -hex 32)
   go run ./cmd/server
   ```

2. Open http://localhost:8080 in two browsers (or one normal and one private
   window), sign up as two people, join `general` and chat.
3. Stop the server with Ctrl+C, start it again, log in, and join `general`.
   Your messages are still there.
4. Look inside the database:

   ```sh
   docker compose exec db psql -U chat -c 'SELECT id, room, user_id, text FROM messages'
   docker compose exec db psql -U chat -c 'SELECT id, username, password_hash FROM users'
   ```

   The hashes start with `$2a$12$`: the bcrypt version, then the cost (12), then
   the salt and hash.
5. Copy your token from DevTools (Network tab, the `/api/login` response) and
   paste it into jwt.io. You can read the payload without the secret. Now change
   one character of the signature and send it to `/api/me` with curl:

   ```sh
   curl -H "Authorization: Bearer <token>" http://localhost:8080/api/me
   ```

6. **Exercise:** add `POST /api/password` that changes your password. It should
   require the current password, validate the new one, and store a new hash.
   Write a contract test for the new `UserStore` method so both stores must
   support it.
7. **Exercise:** add a `migrations/0002_...sql` that adds an `edited_at` column to
   `messages`. Run the server twice and look at `schema_migrations`.
8. **Think about it:** a token is valid for 24 hours and can't be revoked. What
   would you build so that "log out everywhere" works? (Hint: short-lived access
   tokens plus a refresh token stored in the database.)

### Interview questions this chapter prepares you for

- How should passwords be stored? Why bcrypt or Argon2 instead of SHA-256?
- What's in a JWT, and what stops someone from editing it? Is the payload secret?
- What is the `alg: none` attack and how do you prevent it?
- Sessions or JWTs: what are the trade-offs?
- How do you authenticate a WebSocket connection?
- How do you prevent SQL injection?
- Why enforce uniqueness with a database index instead of checking first in code?
- Why is OFFSET pagination slow, and what do you use instead?
- How do you load history and subscribe to live updates without missing messages?
- Why must an event loop never block on I/O?
- What is a Slowloris attack, and which server settings stop it?
- How can a slow password hash become a denial-of-service risk? What is load shedding?
- How does a token bucket rate limiter work?
- Go's `select` picks randomly among ready cases. How can that cause a bug?

**Next chapter:** making the repository professional: CI/CD, linters, security
scanning, coverage, NASA/JPL's "Power of Ten" coding rules adapted to Go, and
release automation.
