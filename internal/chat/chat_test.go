package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/time/rate"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

var testSecret = []byte("test-secret-that-is-at-least-32-bytes-long")

// testEnv is a running chat server plus a way to make login tokens for it.
type testEnv struct {
	srv     *httptest.Server
	handler *Handler
	tokens  *auth.Tokens
	nextID  int64
	stopHub func() // like pressing Ctrl+C on the server
}

// newTestEnv starts a chat server. Its rate limit is switched off, since
// tests send frames much faster than people do; TestFloodingClientIsSlowedDown
// turns it back on.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tokens, err := auth.NewTokens(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	hub := NewHub()
	go hub.Run(done)
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	h := NewHandler(hub, NewMemoryStore(500), tokens)
	h.frameRate = rate.Inf
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); stop() })
	return &testEnv{srv: srv, handler: h, tokens: tokens, stopHub: stop}
}

func (e *testEnv) token(t *testing.T, name string) string {
	t.Helper()
	e.nextID++
	tok, _, err := e.tokens.Issue(auth.User{ID: e.nextID, Username: name})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// rawDial opens a WebSocket without logging in.
func (e *testEnv) rawDial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket: "You never need to close resp.Body yourself."
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// dial connects and logs in as a new user with this name.
func (e *testEnv) dial(t *testing.T, name string) *websocket.Conn {
	t.Helper()
	return e.dialWithToken(t, e.token(t, name))
}

func (e *testEnv) dialWithToken(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	conn := e.rawDial(t)
	write(t, conn, Inbound{Type: TypeAuth, Token: token})
	readUntil(t, conn, isType(TypeReady))
	return conn
}

func write(t *testing.T, c *websocket.Conn, in Inbound) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, in); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readUntil reads frames until one matches, failing after a timeout.
func readUntil(t *testing.T, c *websocket.Conn, match func(Outbound) bool) Outbound {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// jpl:loop returns when a frame matches; a read error (including the
	// 5 s timeout) fails the test.
	for {
		var out Outbound
		if err := wsjson.Read(ctx, c, &out); err != nil {
			t.Fatalf("read: %v", err)
		}
		if match(out) {
			return out
		}
	}
}

// closeCode waits up to 5 s for the server to close the connection and
// returns the close code (-1 if it never sent one).
func closeCode(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// jpl:loop returns on the first read error: the close, or the timeout.
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func isType(typ string) func(Outbound) bool {
	return func(o Outbound) bool { return o.Type == typ }
}

func join(t *testing.T, c *websocket.Conn, room string) Outbound {
	t.Helper()
	write(t, c, Inbound{Type: TypeJoin, Room: room})
	return readUntil(t, c, isType(TypeHistory))
}

func TestMemoryStoreKeepsOnlyRecentMessages(t *testing.T) {
	s := NewMemoryStore(3)
	alice := auth.User{ID: 1, Username: "alice"}
	for i := 1; i <= 5; i++ {
		if _, err := s.SaveMessage(context.Background(), "r", alice, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	msgs, more, _ := s.History(context.Background(), "r", 0, 10)
	if len(msgs) != 3 || msgs[0].Text != "3" || more {
		t.Fatalf("got %+v more=%v", msgs, more)
	}
}

func TestReadyCarriesUser(t *testing.T) {
	e := newTestEnv(t)
	conn := e.rawDial(t)
	write(t, conn, Inbound{Type: TypeAuth, Token: e.token(t, "alice")})
	ready := readUntil(t, conn, isType(TypeReady))
	if ready.User != "alice" || ready.UserID == 0 {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestMessageReachesEveryoneInRoom(t *testing.T) {
	e := newTestEnv(t)
	alice, bob := e.dial(t, "alice"), e.dial(t, "bob")
	join(t, alice, "general")
	join(t, bob, "general")

	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: "hi bob"})
	got := readUntil(t, bob, isType(TypeMessage))
	if got.User != "alice" || got.Text != "hi bob" || got.ID == 0 || got.UserID == 0 {
		t.Fatalf("bob got %+v", got)
	}
	// The sender sees their own message too (that's how the UI confirms it was saved).
	readUntil(t, alice, isType(TypeMessage))
}

func TestRoomsAreIsolated(t *testing.T) {
	e := newTestEnv(t)
	alice, bob := e.dial(t, "alice"), e.dial(t, "bob")
	join(t, alice, "a")
	join(t, bob, "b")

	write(t, alice, Inbound{Type: TypeSend, Room: "a", Text: "secret"})
	readUntil(t, alice, isType(TypeMessage)) // alice's message is fully handled
	write(t, bob, Inbound{Type: TypeSend, Room: "b", Text: "marker"})
	// Bob's next chat message must be his own marker, not Alice's secret.
	if got := readUntil(t, bob, isType(TypeMessage)); got.Text != "marker" {
		t.Fatalf("bob saw a message from another room: %+v", got)
	}
}

func TestLateJoinerGetsHistory(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	join(t, alice, "general")
	for i := 1; i <= 3; i++ {
		write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: fmt.Sprint("msg ", i)})
		readUntil(t, alice, isType(TypeMessage))
	}

	carol := e.dial(t, "carol")
	h := join(t, carol, "general")
	if len(h.Messages) != 3 || h.Messages[2].Text != "msg 3" || h.More {
		t.Fatalf("history = %+v (more=%v)", h.Messages, h.More)
	}
}

func TestHistoryPaging(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	join(t, alice, "general")
	for i := 1; i <= historyPageSize+5; i++ {
		write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: fmt.Sprint(i)})
		readUntil(t, alice, isType(TypeMessage))
	}
	bob := e.dial(t, "bob")
	first := join(t, bob, "general")
	if len(first.Messages) != historyPageSize || !first.More {
		t.Fatalf("first page: %d messages, more=%v", len(first.Messages), first.More)
	}
	write(t, bob, Inbound{Type: TypeHistory, Room: "general", Before: first.Messages[0].ID})
	older := readUntil(t, bob, isType(TypeHistory))
	if len(older.Messages) != 5 || older.More || older.Messages[4].Text != "5" {
		t.Fatalf("older page = %+v (more=%v)", older.Messages, older.More)
	}
}

func TestHistoryRequiresJoin(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	write(t, alice, Inbound{Type: TypeHistory, Room: "general"})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "join") {
		t.Fatalf("got %+v", got)
	}
}

func TestSendWithoutJoinIsRejected(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: "hello?"})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "join") {
		t.Fatalf("got %+v", got)
	}
}

func TestBadFramesGetErrors(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	cases := []struct {
		in   Inbound
		want string
	}{
		{Inbound{Type: "dance"}, "unknown message type"},
		{Inbound{Type: TypeAuth, Token: "x"}, "already authenticated"},
		{Inbound{Type: TypeJoin, Room: "   "}, "room name"},
		{Inbound{Type: TypeJoin, Room: strings.Repeat("r", 65)}, "room name"},
		{Inbound{Type: TypeJoin, Room: "two\nlines"}, "room name"},
		{Inbound{Type: TypeJoin, Room: "red\x1b[31m"}, "room name"}, // a terminal colour code
		{Inbound{Type: TypeJoin, Room: "nul\x00"}, "room name"},
		{Inbound{Type: TypeJoin, Room: "gen\u200beral"}, "room name"}, // a zero-width space
		{Inbound{Type: TypeJoin, Room: "\u202elareneg"}, "room name"}, // shows as "general"
		{Inbound{Type: TypeJoin, Room: "general\u3164"}, "room name"}, // ends with an invisible letter
		{Inbound{Type: TypeJoin, Room: "g\u0435neral"}, "room name"},  // a Cyrillic letter that looks like "e"
		{Inbound{Type: TypeJoin, Room: "game night"}, "room name"},
	}
	for _, c := range cases {
		write(t, alice, c.in)
		if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, c.want) {
			t.Errorf("%+v: got %q, want it to mention %q", c.in, got.Text, c.want)
		}
	}
	join(t, alice, "general")
	texts := map[string]string{
		strings.Repeat("x", maxTextLength+1):   "too long",
		strings.Repeat("é", maxTextLength/2+1): "too long", // the limit counts bytes, and é is 2
		"hi\x00there":                          "invalid characters",
	}
	for text, want := range texts {
		write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: text})
		if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, want) {
			t.Errorf("send %.20q: got %q, want it to mention %q", text, got.Text, want)
		}
	}
}

func TestInvalidJSONClosesTheConnection(t *testing.T) {
	e := newTestEnv(t)
	for name, frame := range map[string]string{
		"not JSON":            `{"type": "send", "te`,
		"wrong type of value": `{"type": 42}`,
		"not an object":       `["join", "general"]`,
	} {
		t.Run(name, func(t *testing.T) {
			conn := e.dial(t, "alice")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				t.Fatal(err)
			}
			if code := closeCode(t, conn); code != websocket.StatusInvalidFramePayloadData {
				t.Fatalf("close code = %v, want %v", code, websocket.StatusInvalidFramePayloadData)
			}
		})
	}
}

// TestFloodingClientIsSlowedDown checks the per-connection rate limit: a
// client that sends too fast gets an error instead of service, and other
// people are not affected.
func TestFloodingClientIsSlowedDown(t *testing.T) {
	e := newTestEnv(t)
	e.handler.frameRate = rate.Every(time.Hour) // no refills during the test
	e.handler.frameBurst = 3
	alice, bob := e.dial(t, "alice"), e.dial(t, "bob")
	join(t, bob, "general")
	join(t, alice, "general") // alice's frame 1
	for i := 2; i <= 3; i++ {
		write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: fmt.Sprint("msg ", i)})
		readUntil(t, alice, isType(TypeMessage))
	}
	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: "one too many"})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "slow down") {
		t.Fatalf("got %+v", got)
	}

	// Bob has his own allowance. He can still talk, and the refused
	// message never reached him (it would have arrived before his own).
	write(t, bob, Inbound{Type: TypeSend, Room: "general", Text: "bob is fine"})
	readUntil(t, bob, func(o Outbound) bool {
		if o.Text == "one too many" {
			t.Error("a refused message was delivered")
		}
		return o.Type == TypeMessage && o.User == "bob"
	})
}

func TestTooManyRooms(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	for i := range maxRoomsPerConn {
		join(t, alice, fmt.Sprint("room-", i))
	}
	write(t, alice, Inbound{Type: TypeJoin, Room: "one-too-many"})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "too many rooms") {
		t.Fatalf("got %+v", got)
	}
}

func TestLeaveStopsMessagesAndTellsOthers(t *testing.T) {
	e := newTestEnv(t)
	alice, bob := e.dial(t, "alice"), e.dial(t, "bob")
	join(t, alice, "general")
	join(t, bob, "general")
	write(t, bob, Inbound{Type: TypeLeave, Room: "general"})
	left := readUntil(t, alice, func(o Outbound) bool { return o.Type == TypeSystem && strings.Contains(o.Text, "left") })
	if left.User != "bob" {
		t.Fatalf("got %+v", left)
	}
	write(t, bob, Inbound{Type: TypeSend, Room: "general", Text: "still here?"})
	if got := readUntil(t, bob, isType(TypeError)); !strings.Contains(got.Text, "join") {
		t.Fatalf("got %+v", got)
	}
}

func TestDisconnectTellsOthers(t *testing.T) {
	e := newTestEnv(t)
	alice, bob := e.dial(t, "alice"), e.dial(t, "bob")
	join(t, alice, "general")
	join(t, bob, "general")
	if err := bob.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, alice, func(o Outbound) bool {
		return o.Type == TypeSystem && o.User == "bob" && strings.Contains(o.Text, "left")
	})
}

func TestAuthIsRequired(t *testing.T) {
	e := newTestEnv(t)
	e.handler.authTimeout = 300 * time.Millisecond

	expired, err := auth.NewTokens(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	oldToken, _, _ := expired.Issue(auth.User{ID: 1, Username: "alice"})

	otherKey, _ := auth.NewTokens([]byte("a-completely-different-secret-of-32-bytes"), time.Hour)
	forged, _, _ := otherKey.Issue(auth.User{ID: 1, Username: "alice"})

	cases := map[string]*Inbound{
		"no auth frame":        nil,
		"join before auth":     {Type: TypeJoin, Room: "general"},
		"garbage token":        {Type: TypeAuth, Token: "not-a-jwt"},
		"expired token":        {Type: TypeAuth, Token: oldToken},
		"token from other key": {Type: TypeAuth, Token: forged},
		"auth without a token": {Type: TypeAuth},
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			conn := e.rawDial(t)
			if first != nil {
				write(t, conn, *first)
			}
			if code := closeCode(t, conn); code != StatusUnauthorized {
				t.Fatalf("close code = %v, want %v", code, StatusUnauthorized)
			}
		})
	}
}

func TestConnectionClosesWhenTokenExpires(t *testing.T) {
	e := newTestEnv(t)
	short, err := auth.NewTokens(testSecret, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := short.Issue(auth.User{ID: 7, Username: "alice"})
	conn := e.dialWithToken(t, tok)
	if code := closeCode(t, conn); code != StatusUnauthorized {
		t.Fatalf("close code = %v, want %v", code, StatusUnauthorized)
	}
}

// TestManyConcurrentClients is the concurrency test: 50 clients all send at
// once, and every client must receive every message. Run with -race to let
// Go's race detector check for unsafe shared memory.
func TestManyConcurrentClients(t *testing.T) {
	const n = 50
	e := newTestEnv(t)
	conns := make([]*websocket.Conn, n)
	for i := range conns {
		conns[i] = e.dial(t, fmt.Sprint("user", i))
		join(t, conns[i], "load")
	}

	// Everyone sends at once, and everyone reads at once. Each goroutine
	// writes only its own slot of errs, so they need no lock.
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = wsjson.Write(ctx, c, Inbound{Type: TypeSend, Room: "load", Text: fmt.Sprint("from ", i)})
		})
		wg.Go(func() {
			if err := expectEachMessageOnce(c, n); err != nil {
				errs[i] = fmt.Errorf("client %d: %w", i, err)
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

// expectEachMessageOnce reads until it has seen n different chat messages,
// and fails if any message arrives twice.
func expectEachMessageOnce(c *websocket.Conn, n int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := map[int64]bool{}
	for len(seen) < n {
		var out Outbound
		if err := wsjson.Read(ctx, c, &out); err != nil {
			return fmt.Errorf("saw %d/%d messages: %w", len(seen), n, err)
		}
		if out.Type != TypeMessage {
			continue
		}
		if seen[out.ID] {
			return fmt.Errorf("got message %d twice", out.ID)
		}
		seen[out.ID] = true
	}
	return nil
}

// TestShutdownSaysGoingAway checks that stopping the server closes chat
// connections with code 1001 ("going away"), which tells the browser it
// may reconnect, instead of just dropping them.
func TestShutdownSaysGoingAway(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	join(t, alice, "general")
	e.stopHub()
	if code := closeCode(t, alice); code != websocket.StatusGoingAway {
		t.Fatalf("close code = %v, want %v", code, websocket.StatusGoingAway)
	}
}

// TestWaitLetsConnectionsSayGoodbye: after the hub stops, Wait must not
// return while a connection is still saying goodbye, or the server could
// exit before browsers learn why they were disconnected. Here the client
// doesn't read at first, so the server's close handshake can't finish yet
// and Wait has to keep waiting. Once the client reads, it gets 1001 and
// answers, and Wait returns.
func TestWaitLetsConnectionsSayGoodbye(t *testing.T) {
	e := newTestEnv(t)
	alice := e.dial(t, "alice")
	join(t, alice, "general")
	e.stopHub()

	waited := make(chan error, 1)
	go func() { waited <- e.handler.hub.Wait(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatalf("Wait returned (%v) while a connection was still closing", err)
	case <-time.After(300 * time.Millisecond):
	}
	if code := closeCode(t, alice); code != websocket.StatusGoingAway {
		t.Fatalf("close code = %v, want %v", code, websocket.StatusGoingAway)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the connection closed")
	}
}

// TestWaitGivesUpWhenItsContextEnds: Wait is bounded by its context, so a
// connection that never finishes can't keep the server from exiting.
func TestWaitGivesUpWhenItsContextEnds(t *testing.T) {
	h := NewHub()
	done := make(chan struct{})
	go h.Run(done)
	t.Cleanup(func() { close(done) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait on a running hub returned %v, want %v", err, context.DeadlineExceeded)
	}
}

// The tests below drive the hub directly, without goroutines or sockets,
// so they can set up exact situations that are hard to time with real
// connections.

func newTestClient(name string, buffer int) *Client {
	return &Client{
		user: auth.User{Username: name}, send: make(chan Outbound, buffer),
		rooms: map[string]bool{}, joined: map[string]bool{},
	}
}

// drain returns what is waiting in c's buffer, as "type:text" strings.
func drain(c *Client) string {
	var got []string
	for len(c.send) > 0 {
		m := <-c.send
		got = append(got, m.Type+":"+m.Text)
	}
	return strings.Join(got, ",")
}

// TestSlowClientIsDropped checks the backpressure rule: a client whose
// buffer is full gets removed, and everyone else still gets the message.
func TestSlowClientIsDropped(t *testing.T) {
	h := NewHub()
	fast, slow := newTestClient("fast", 100), newTestClient("slow", 1)
	for _, c := range []*Client{fast, slow} {
		h.apply(command{kind: cmdRegister, client: c})
	}
	h.apply(command{kind: cmdJoin, client: slow, room: "r"}) // fills slow's 1-slot buffer
	h.apply(command{kind: cmdJoin, client: fast, room: "r"}) // slow can't take "fast joined": dropped

	if h.clients[slow] || h.rooms["r"][slow] {
		t.Fatal("slow client should have been removed")
	}
	// slow's channel is closed after its one buffered message.
	<-slow.send
	if _, open := <-slow.send; open {
		t.Fatal("slow client's channel should be closed")
	}
	if slow.goodbye != byeSlow {
		t.Fatalf("goodbye = %+v, want %+v", slow.goodbye, byeSlow)
	}
	// A command still queued from the removed client must be ignored, not panic.
	h.apply(command{kind: cmdDirect, client: slow, msg: Outbound{Type: TypeError}})
	h.apply(command{kind: cmdJoin, client: slow, room: "r"})

	h.apply(command{kind: cmdBroadcast, room: "r", msg: Outbound{Type: TypeMessage, Text: "hi"}})
	if got, want := drain(fast), "system:fast joined,system:slow left,message:hi"; got != want {
		t.Fatalf("fast got %v, want %v", got, want)
	}
}

// TestSlowClientsAreDroppedInACascade: removing a client announces "left"
// in each of its rooms, clients too slow to take that announcement are
// removed too, their own "left" announcements can find more slow clients,
// and so on (see removeClient). Here two slow clients share two rooms with
// a client that leaves. Everyone except the fast client must end up
// removed, each exactly once (closing a channel twice would panic), and
// the fast client must hear every departure once in each room.
func TestSlowClientsAreDroppedInACascade(t *testing.T) {
	h := NewHub()
	leaver, slow1, slow2, fast := newTestClient("leaver", 10), newTestClient("slow1", 10),
		newTestClient("slow2", 10), newTestClient("fast", 10)
	for _, c := range []*Client{leaver, slow1, slow2, fast} {
		h.apply(command{kind: cmdRegister, client: c})
		h.apply(command{kind: cmdJoin, client: c, room: "a"})
		h.apply(command{kind: cmdJoin, client: c, room: "b"})
	}
	drain(fast)
	fill(slow1)
	fill(slow2)

	h.apply(command{kind: cmdUnregister, client: leaver})

	if len(h.clients) != 1 || len(h.rooms["a"]) != 1 || len(h.rooms["b"]) != 1 {
		t.Fatalf("%d clients are still connected, %d and %d in the rooms; want only fast",
			len(h.clients), len(h.rooms["a"]), len(h.rooms["b"]))
	}
	checkRemoved(t, leaver, byeNormal)
	checkRemoved(t, slow1, byeSlow)
	checkRemoved(t, slow2, byeSlow)
	heard := strings.Split(drainWithRooms(fast), ",")
	slices.Sort(heard) // who goes first depends on map order
	want := []string{
		"a: leaver left", "a: slow1 left", "a: slow2 left",
		"b: leaver left", "b: slow1 left", "b: slow2 left",
	}
	if !slices.Equal(heard, want) {
		t.Fatalf("fast heard %q, want %q", heard, want)
	}
}

// fill fills c's buffer, so one more message is too many.
func fill(c *Client) {
	for len(c.send) < cap(c.send) {
		c.send <- Outbound{}
	}
}

// checkRemoved checks that the hub closed c's channel, after the messages
// already in it, and recorded why.
func checkRemoved(t *testing.T, c *Client, why closeReason) {
	t.Helper()
	drain(c)
	select {
	case _, open := <-c.send:
		if open {
			t.Errorf("%s got a message after being removed", c.user.Username)
		}
	default:
		t.Errorf("%s's channel is still open", c.user.Username)
	}
	if c.goodbye != why {
		t.Errorf("%s's goodbye = %+v, want %+v", c.user.Username, c.goodbye, why)
	}
}

// drainWithRooms is drain, with each message's room in front of its text.
func drainWithRooms(c *Client) string {
	var got []string
	for len(c.send) > 0 {
		m := <-c.send
		got = append(got, m.Room+": "+m.Text)
	}
	return strings.Join(got, ",")
}

// TestSavedMessageIsDeliveredAfterSenderLeaves: once a message is saved
// to the database it must reach the room, even if the sender closed the
// tab between saving and broadcasting. Otherwise the message would show
// up in history later but never live.
func TestSavedMessageIsDeliveredAfterSenderLeaves(t *testing.T) {
	h := NewHub()
	alice, bob := newTestClient("alice", 10), newTestClient("bob", 10)
	for _, c := range []*Client{alice, bob} {
		h.apply(command{kind: cmdRegister, client: c})
		h.apply(command{kind: cmdJoin, client: c, room: "r"})
	}
	drain(bob)
	h.apply(command{kind: cmdUnregister, client: alice}) // alice's tab closes...
	h.apply(command{kind: cmdBroadcast, room: "r", msg: Outbound{Type: TypeMessage, Text: "bye"}})
	if got, want := drain(bob), "system:alice left,message:bye"; got != want {
		t.Fatalf("bob got %v, want %v", got, want)
	}
}

// TestSubmitNeverBlocksAfterHubStops: once the hub has stopped, nobody
// reads its queue. submit must then say no straight away instead of
// waiting forever, or every connection still open would hang.
func TestSubmitNeverBlocksAfterHubStops(t *testing.T) {
	h := NewHub()
	done := make(chan struct{})
	go h.Run(done)
	close(done)
	<-h.stopped

	finished := make(chan bool)
	go func() {
		accepted := false
		for range cap(h.commands) + 10 { // more than the queue holds
			accepted = h.submit(command{kind: cmdDirect}) || accepted
		}
		finished <- accepted
	}()
	select {
	case accepted := <-finished:
		if accepted {
			t.Fatal("a stopped hub accepted a command")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit blocked after the hub stopped")
	}
}
