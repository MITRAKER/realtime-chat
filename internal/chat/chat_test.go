package chat

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

var testSecret = []byte("test-secret-that-is-at-least-32-bytes-long")

// testEnv is a running chat server plus a way to make login tokens for it.
type testEnv struct {
	srv     *httptest.Server
	handler *Handler
	tokens  *auth.Tokens
	nextID  int64
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tokens, err := auth.NewTokens(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	hub := NewHub()
	go hub.Run(done)
	h := NewHandler(hub, NewMemoryStore(500), tokens)
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); close(done) })
	return &testEnv{srv: srv, handler: h, tokens: tokens}
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
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
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

// closeCode waits for the server to close the connection and returns the code.
func closeCode(t *testing.T, c *websocket.Conn, wait time.Duration) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
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
		s.SaveMessage(context.Background(), "r", alice, fmt.Sprint(i))
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
	}
	for _, c := range cases {
		write(t, alice, c.in)
		if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, c.want) {
			t.Errorf("%+v: got %q, want it to mention %q", c.in, got.Text, c.want)
		}
	}
	join(t, alice, "general")
	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: strings.Repeat("x", maxTextLength+1)})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "too long") {
		t.Errorf("long message: got %q", got.Text)
	}
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
	bob.Close(websocket.StatusNormalClosure, "bye")
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
			code := closeCode(t, conn, 5*time.Second)
			// With no frame at all, the read deadline drops the connection
			// without a close frame; either way it must not stay open.
			if first != nil && code != StatusUnauthorized {
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
	if code := closeCode(t, conn, 5*time.Second); code != StatusUnauthorized {
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

	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = wsjson.Write(ctx, c, Inbound{Type: TypeSend, Room: "load", Text: fmt.Sprint("from ", i)})
		}()
	}

	// Every client should see all n messages, each exactly once.
	errs := make(chan error, n)
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			seen := map[int64]bool{}
			for len(seen) < n {
				var out Outbound
				if err := wsjson.Read(ctx, c, &out); err != nil {
					errs <- fmt.Errorf("client %d saw %d/%d messages: %w", i, len(seen), n, err)
					return
				}
				if out.Type == TypeMessage {
					if seen[out.ID] {
						errs <- fmt.Errorf("client %d got message %d twice", i, out.ID)
						return
					}
					seen[out.ID] = true
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestSlowClientIsDropped drives the hub directly, without goroutines or
// sockets, to check the backpressure rule: a client whose buffer is full
// gets removed, and everyone else still gets the message.
func TestSlowClientIsDropped(t *testing.T) {
	h := NewHub()
	newClient := func(name string, buffer int) *Client {
		return &Client{
			user: auth.User{Username: name}, send: make(chan Outbound, buffer),
			rooms: map[string]bool{}, joined: map[string]bool{},
		}
	}
	fast, slow := newClient("fast", 100), newClient("slow", 1)
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
	// A command still queued from the removed client must be ignored, not panic.
	h.apply(command{kind: cmdDirect, client: slow, msg: Outbound{Type: TypeError}})
	h.apply(command{kind: cmdJoin, client: slow, room: "r"})

	h.apply(command{kind: cmdBroadcast, room: "r", msg: Outbound{Type: TypeMessage, Text: "hi"}})
	var got []string
	for len(fast.send) > 0 {
		m := <-fast.send
		got = append(got, m.Type+":"+m.Text)
	}
	want := "system:fast joined,system:slow left,message:hi"
	if strings.Join(got, ",") != want {
		t.Fatalf("fast got %v, want %v", got, want)
	}
}
