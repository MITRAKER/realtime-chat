# Changelog

Every notable change to this project is written down here, newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before version 1.0.0, a minor version (0.**x**.0) may change how things work.

## [Unreleased]

Nothing has been released yet. Step 3 adds the release workflow, so it will
be the first release, v0.3.0, once its pull request is merged and tagged (see
"Releases" in [CONTRIBUTING.md](CONTRIBUTING.md)). Steps 1 and 2 came before
that workflow existed: tagging them v0.1.0 and v0.2.0 would only mark them in
the history, with no release notes, binaries or image.

### Step 3 (v0.3.0): a professional repository

#### Added

- CI on every push and pull request: `go mod tidy` check, `go vet`,
  golangci-lint with about 50 linters, the NASA/JPL rule checker, ShellCheck,
  tests with the race detector and a real PostgreSQL on the two supported Go
  releases, a coverage minimum of 80%, `govulncheck`, a rehearsal of the
  release (GoReleaser builds and packs the binaries for five platforms without
  publishing them), and a build of the Docker image.
  One "CI OK" check sums it all up for branch protection.
- A daily scheduled run of static analysis, the vulnerability scan and the
  fuzz tests, so new problems are found even when nobody pushes.
- `jplcheck`, a custom static analyzer for NASA/JPL "Power of Ten" rules 1
  and 2: no `goto`, no recursion (direct or through other functions), and
  every loop that can run forever (no condition, an always-true one, or a
  range over a channel or a function iterator) must say in a `jpl:loop`
  comment how it ends.
- Seven fuzz tests for the input strangers control: WebSocket frames, the
  sign-up and login API, login tokens, usernames, passwords, and history
  paging.
- CodeQL code scanning, dependency review on pull requests, and Dependabot
  updates for Go modules, GitHub Actions and the Docker base image.
- A release pipeline: pushing a `v*.*.*` tag tests the code, publishes
  binaries for Linux, macOS and Windows with checksums (GoReleaser), and
  pushes a multi-platform Docker image to `ghcr.io/mitraker/realtime-chat`
  with an SBOM and build provenance.
- A Dockerfile that builds a small image running as a non-root user.
- A Makefile (`make help` lists the tasks), pre-commit hooks (including a
  secret scanner), `.editorconfig`, issue and pull request templates,
  CODEOWNERS, CONTRIBUTING.md, SECURITY.md, CODE_OF_CONDUCT.md and
  [docs/CODING_STANDARDS.md](docs/CODING_STANDARDS.md).
- The server logs its version at startup (set at build time with
  `-ldflags "-X main.version=..."`).
- Tests for `cmd/server`: configuration, failing fast on bad settings,
  starting, and shutting down gracefully.
- LEARNING.md chapter 3.

#### Changed

- Room names may only use ASCII letters, digits, `_` and `-`, like
  usernames. Before, almost any character was allowed, so someone could make
  a room that looks exactly like `general` using invisible characters (such
  as U+200B or U+3164), a right-to-left override (U+202E) or a Cyrillic
  letter that looks Latin. Rooms with other names can no longer be joined.
- Passwords may not contain the NUL character. An account made earlier with
  one in its password can no longer log in (no release ever allowed it).
- The hub removes disconnected and slow clients with a work queue instead of
  a recursive call (JPL rule 1). Each client enters the queue at most once,
  so the memory it needs is bounded by the number of clients.
- The server opens its port before the slow setup, so a port that is already
  taken fails at once.
- The Docker image is built with the current stable Go release.
- Code and tests were reworked until all linters and `jplcheck` report
  nothing: every error is checked, every function is short, and long
  functions were split.

#### Fixed

- On shutdown the server could exit before telling chat connections why they
  were closing, so many browsers saw the connection drop (1006) instead of
  getting `1001` ("going away", reconnect in a moment). The server now waits,
  within the same 10 seconds, until every chat connection has closed
  properly. A connection that arrives during shutdown also gets `1001`.

#### Security

- Fixed: an account whose password was exactly 72 bytes long could also be
  opened with that password followed by anything. bcrypt only reads the first
  72 bytes, and login didn't check the length. Likewise, because bcrypt
  repeats short keys, `password` and `password\x00password` were the same
  password. Login now refuses passwords that bcrypt can't tell apart. The
  `FuzzLogin` fuzz test found the first case by itself in under a minute.

### Step 2 (v0.2.0): accounts, login tokens and PostgreSQL

#### Added

- Sign-up and login (`POST /api/register`, `POST /api/login`), with passwords
  hashed by bcrypt, and `GET /api/me`.
- Login tokens: JSON Web Tokens signed with HMAC-SHA256, with an expiry.
- WebSocket login: the first frame must carry a valid token within 5 seconds.
- PostgreSQL storage for accounts and messages, with migrations that are safe
  when several servers start at once, and history paging by message id.
- `/healthz` and `/readyz` endpoints, configuration from environment
  variables, and a Docker Compose file for a local PostgreSQL.

#### Security

- Logins for unknown usernames take as long as wrong passwords, so the login
  form doesn't reveal which usernames exist.
- At most half the CPU cores hash passwords at once; when they're all busy,
  logins get `503` with `Retry-After` instead of freezing the chat.
- Each WebSocket connection may send 10 frames per second (bursts of 20).
- HTTP timeouts protect against clients that send very slowly (Slowloris).
- Input with NUL or control characters is refused before it reaches the
  database.
- WebSocket close codes say why a connection ended (`1001`, `1007`, `1008`,
  `4401`).

#### Fixed

- A message saved just as its sender disconnected was sometimes never
  delivered live.

### Step 1 (v0.1.0): WebSocket chat hub

#### Added

- A WebSocket chat server with rooms: join, leave, send, and load older
  messages.
- One goroutine (the hub) owns all shared state; each connection has a
  reader and a writer goroutine.
- Slow clients are dropped instead of slowing everyone down.
- Keep-alive pings, graceful shutdown, and limits on frame size, message
  length, room name length and rooms per connection.
- In-memory history of the last 500 messages per room.
- A browser test page, and tests that run with the race detector.

[Unreleased]: https://github.com/MITRAKER/realtime-chat/commits/main
