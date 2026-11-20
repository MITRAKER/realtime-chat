# Coding standards

This project follows **"The Power of Ten: Rules for Developing Safety-Critical
Code"** by Gerard J. Holzmann of NASA's Jet Propulsion Laboratory (JPL), 2006.
JPL wrote the rules for C code that flies on spacecraft, where a crash can't be
fixed by restarting. A chat server is not a Mars rover, but the rules target the
bugs that are hardest to find in any language: runaway loops, unchecked errors,
code too complex to reason about. They are also an excellent habit to show in
interviews.

Some rules don't translate directly to Go. For each rule below you'll find the
original, how it applies to Go, and **what enforces it**. A rule that only lives
in a document gets forgotten, so as many as possible are checked automatically
on every pull request.

| # | JPL rule (short) | In this repository | Enforced by |
|---|------------------|--------------------|-------------|
| 1 | Simple control flow: no `goto`, no recursion | Same. Also limit nesting and complexity | `jplcheck`, `nestif`, `gocognit`, `gocyclo` |
| 2 | Every loop has a fixed upper bound | Loops over data are bounded by limits. Loops that run "until shut down" must document how they end | `jplcheck` (`jpl:loop` comments), code review |
| 3 | No dynamic memory allocation after start-up | Go needs the heap, so instead every queue, buffer and input has a hard maximum size; connection counts get theirs in step 5 | limits table below, tests, review |
| 4 | Functions fit on one page (about 60 lines) | Max 60 lines and 40 statements | `funlen` |
| 5 | At least two assertions per function | Validate every input at trust boundaries; check invariants explicitly; fuzz tests | review, `go test -fuzz` |
| 6 | Declare data at the smallest possible scope | Same; package-level state only for constants, errors and embedded files | compiler, `ineffassign`, `wastedassign`, review |
| 7 | Check every return value and every parameter | Every `error` is handled; ignoring one needs an explicit `_ =` | `errcheck`, `nilerr`, `errorlint`, review |
| 8 | Limit the preprocessor | Go has none. No cgo, no `unsafe`, no build-tag variants | `CGO_ENABLED=0` builds, `gosec` (G103) |
| 9 | Limit pointers; no function pointers | Go has no pointer arithmetic. Function values only for handlers, callbacks and injected dependencies | `gosec`, review |
| 10 | All warnings on, zero warnings, daily static analysis | CI fails on any finding (CodeQL: on serious new alerts); analysis and vulnerability scans also run every day | CI, `golangci-lint`, `go vet`, CodeQL, `govulncheck` |

## Rule 1: simple control flow

> Restrict all code to very simple control flow constructs: do not use `goto`
> statements, `setjmp` or `longjmp` constructs, or direct or indirect recursion.

**Why:** recursion makes the stack depth depend on the input, so you can't
prove the program won't run out of stack. It also makes the call graph cyclic,
which defeats many analysis tools.

**In Go:** no `goto` and no recursion, direct (`f` calls `f`) or indirect (`f`
calls `g` calls `f`). Iterate with an explicit work list instead (see
`removeClient` in `internal/chat/hub.go`, which keeps a queue of clients
still to remove, each at most once, and Tarjan's algorithm in
`internal/jplcheck`, which itself contains no recursion). We also cap nesting
(`nestif`) and complexity (`gocognit` 15, `gocyclo` 12).

**Checked by** `go run ./cmd/jplcheck ./...`. It builds each package's call
graph from the type information and reports every group of functions that
call each other in a loop, with one example chain of calls per group. Calls
through interfaces or function values can't be resolved statically, so review
must cover those.

## Rule 2: bounded loops

> All loops must have a fixed upper bound. It must be trivially possible for a
> checking tool to prove statically that a preset upper bound on the number of
> iterations of a loop cannot be exceeded.

**Why:** a loop that can spin forever hangs the program.

**In Go:** loops over data are naturally bounded (ranging over a slice, or a
counter compared to a limit). A server also has loops that are *meant* to run
until shutdown: the hub's event loop, each connection's read and write loops.
Holzmann exempts loops that are meant never to end, but for those he asks for
the reverse proof: that they *can't* end. Ours do end, at shutdown, so neither
version fits, and this repository relaxes the rule for them. Instead of a
fixed bound, every loop that has no condition (or one that is always true),
or ranges over a channel or a function iterator, must have a comment directly
above it (or above its label) that starts with `jpl:loop` and says how it ends:

```go
// jpl:loop ends when done is closed (server shutdown).
for {
```

**Checked by** `jplcheck`, which rejects such a loop without the comment.

## Rule 3: no dynamic allocation after initialization

> Do not use dynamic memory allocation after initialization.

**Why:** allocators and garbage collectors have unpredictable timing, and
memory that grows without limit eventually runs out.

**In Go:** the garbage collector is not optional, so we keep the spirit:
**nothing may grow without a limit.** Every buffer, queue and input size has a
hard maximum, so no single message, request or connection can take more than
its share. The last two rows are the gaps that remain: until step 5 adds
per-user limits, someone who opens enough connections can still make the
server run out of memory.

| Limit | Value | Where |
|-------|-------|-------|
| WebSocket frame size | 8 KB | `maxFrameSize`, `internal/chat/client.go` |
| Chat message length | 2000 bytes | `maxTextLength` |
| Room name | 1-64 letters, digits, `_` or `-` | `roomPattern`, `maxRoomName` |
| Rooms per connection | 100 | `maxRoomsPerConn` |
| Messages queued per client | 64 | `sendBuffer` (slow clients are dropped) |
| History page size | 50 | `historyPageSize` |
| Frames per connection | 10 per second, bursts of 20 | `defaultFrameRate`, `internal/chat/handler.go` |
| Time to send the login token | 5 s | `defaultAuthTimeout` |
| Hub command queue | 1024 | `hubQueueSize`, `internal/chat/hub.go` |
| Clients waiting to be removed | each client at most once | `queued` in `removeClient` |
| HTTP JSON body | 4 KB | `maxBodyBytes`, `internal/auth/http.go` |
| Username length | 3-32 bytes | `usernamePattern`, `internal/auth/user.go` |
| Password length | 8 characters to 72 bytes | `MinPasswordLength`, `MaxPasswordBytes` |
| Password hashes running at once | half the CPU cores | `NewService`, `internal/auth/service.go` |
| HTTP header, request and idle time | 5 s, 15 s, 60 s | `DefaultTimeouts`, `internal/app/app.go` |
| Database connections | pool maximum | `pool_max_conns` in `DATABASE_URL` |
| In-memory history per room | 500 | `NewMemoryStore(500)`, `cmd/server/main.go` |
| Shutdown (requests and chat goodbyes) | 10 s | `shutdownTimeout`, `cmd/server/main.go` |
| Connections, per user and in total | **none yet** (step 5) | `Handler.ServeHTTP`, `internal/chat/handler.go` |
| Accounts and rooms without PostgreSQL | **none**: every one stays in memory | `MemoryUserStore`, `MemoryStore` (production uses `DATABASE_URL`) |

A new limit goes in this table.

## Rule 4: short functions

> No function should be longer than what can be printed on a single sheet of
> paper: about 60 lines.

**Why:** a function you can see all at once is a function you can understand
and test.

**Checked by** `funlen` (60 lines, 40 statements) plus `gocognit` and
`gocyclo` for complexity. Tests follow the same limits: a test that doesn't
fit on a page is usually testing several things and reads better split up.

## Rule 5: assertions

> The assertion density of the code should average a minimum of two assertions
> per function. Assertions must be side-effect free and should be defined as
> Boolean tests. When an assertion fails, an explicit recovery action must be
> taken.

**Why:** an assertion catches the moment something impossible happens,
instead of letting bad data travel and crash something unrelated later.

**In Go** there is no `assert` keyword, and a server should recover instead of
crashing. So:

- **Every input from outside is validated** where it enters: JSON bodies,
  WebSocket frames, tokens, environment variables. Failure has an explicit
  recovery: an error response, a close code, or refusing to start.
- **Internal invariants are checked explicitly**, and a violation is handled.
  Example: the hub ignores commands from clients it already removed, instead of
  sending on a closed channel.
- **Fuzz tests** (`Fuzz*` functions) feed millions of generated inputs to the
  code that handles outside input, and check a property on every result.
  This is assertion checking at scale; CI runs them every day:

  | Fuzz test | Property |
  |-----------|----------|
  | `FuzzHandleFrame` | any WebSocket frame gets a sensible reply; nothing invalid is ever saved |
  | `FuzzAccountAPI` | any sign-up or login body gets a planned answer (never a 500) in JSON |
  | `FuzzLogin` | a login works with the exact password and nothing else |
  | `FuzzParseToken` | the token parser never crashes and never accepts a token we didn't sign |
  | `FuzzTokenRoundTrip` | every token we issue reads back as the same user |
  | `FuzzValidateUsername` | the username regular expression matches the rule written in plain code |
  | `FuzzMemoryStoreHistory` | history paging agrees with a slow, obviously correct model |

  `FuzzLogin` found a real bug in its first minute (see CHANGELOG.md).

## Rule 6: smallest scope

> Data objects must be declared at the smallest possible level of scope.

**In Go:** declare variables where they're used (`if err := ...; err != nil`),
pass dependencies explicitly (see `app.Deps`), and keep package-level variables
to constants, sentinel errors, compiled regular expressions and embedded files.
There is no global mutable state.

**Checked by** the compiler (unused variables don't compile), `ineffassign`,
`wastedassign`, and review.

## Rule 7: check return values and parameters

> The return value of non-void functions must be checked by each calling
> function, and the validity of parameters must be checked inside each function.

**In Go:** every returned `error` is handled. When ignoring one is truly
correct (for example, closing a connection that is already failing), say so
explicitly with `_ =` and a comment. That's the Go version of JPL's `(void)`
cast. Errors are compared with `errors.Is` and `errors.As`, never with `==`
or string matching, so wrapped errors still match.

**Checked by** `errcheck` (with type assertions), `errorlint`, `nilerr`,
`nilnesserr`, `rowserrcheck`, `sqlclosecheck`, `bodyclose`.

## Rule 8: limit the preprocessor

> The use of the preprocessor must be limited to the inclusion of header files
> and simple macro definitions.

**In Go:** there is no preprocessor. The nearest things are cgo, `unsafe` and
build tags that compile different code on different platforms. We use none of
them: release binaries are built with `CGO_ENABLED=0`, `gosec` flags `unsafe`,
and the same code runs everywhere.

## Rule 9: limit pointers

> The use of pointers should be restricted. Specifically, no more than one level
> of dereferencing is allowed. Function pointers are not permitted.

**In Go:** there is no pointer arithmetic, and the garbage collector prevents
dangling pointers, so most of the danger is gone. We avoid pointer-to-pointer
types. Function values are used for HTTP handlers and other callbacks, and to
pass in what a test needs to replace: `run` takes `getenv` instead of reading
the environment itself, the hub and the token code use a replaceable clock,
and `serve` takes the `drain` function it calls at shutdown. Every call
through a function value is a call the recursion checker can't see, so keep
them few and simple, and check them in review for calls that loop back.

## Rule 10: zero warnings, analyzed daily

> All code must be compiled, from the first day of development, with all
> compiler warnings enabled. All code must compile without warnings. All code
> must be checked daily with at least one, but preferably more than one, static
> source code analyzer and should pass the analyses with zero warnings.

**In Go:** CI fails on any finding from `go vet`, `golangci-lint` (about 50
linters, including `staticcheck` and `gosec`), `jplcheck`, ShellCheck and
`govulncheck`. There is no "warning" level that can be ignored. CodeQL works
differently: all its results go to the repository's Security tab, but by
default its pull request check fails only when the change adds an alert of
severity "error" or a security alert rated high or critical. (The code
scanning settings can make it stricter.) A scheduled run every day repeats
the analysis and the vulnerability scan, and runs the fuzz tests; CodeQL also
runs weekly.

If a linter is wrong about a specific line, silence it on that line only, with
a reason:

```go
//nolint:gosec // G404: math/rand is fine here, this picks a color, not a key
```

`nolintlint` rejects a `//nolint` without a specific linter and a reason.

## Reference

Gerard J. Holzmann, "The Power of Ten: Rules for Developing Safety-Critical
Code", IEEE Computer, June 2006.
