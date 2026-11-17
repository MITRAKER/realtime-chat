package chat

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// startServer runs a real hub and HTTP server for a test.
func startServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	hub := NewHub(NewMemoryStore(100))
	go hub.Run(done)
	srv := httptest.NewServer(NewServer(hub, fstest.MapFS{}))
	t.Cleanup(func() { srv.Close(); close(done) })
	return srv
}

func dial(t *testing.T, srv *httptest.Server, name string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?name=" + name
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
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

func isType(typ string) func(Outbound) bool {
	return func(o Outbound) bool { return o.Type == typ }
}

func TestMessageReachesEveryoneInRoom(t *testing.T) {
	srv := startServer(t)
	alice, bob := dial(t, srv, "alice"), dial(t, srv, "bob")

	write(t, alice, Inbound{Type: TypeJoin, Room: "general"})
	readUntil(t, alice, isType(TypeHistory))
	write(t, bob, Inbound{Type: TypeJoin, Room: "general"})
	readUntil(t, bob, isType(TypeHistory))

	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: "hi bob"})
	got := readUntil(t, bob, isType(TypeMessage))
	if got.User != "alice" || got.Text != "hi bob" {
		t.Fatalf("bob got %+v", got)
	}
	// The sender sees their own message too (that's how the UI confirms it was saved).
	readUntil(t, alice, isType(TypeMessage))
}

func TestRoomsAreIsolated(t *testing.T) {
	srv := startServer(t)
	alice, bob := dial(t, srv, "alice"), dial(t, srv, "bob")
	write(t, alice, Inbound{Type: TypeJoin, Room: "a"})
	readUntil(t, alice, isType(TypeHistory))
	write(t, bob, Inbound{Type: TypeJoin, Room: "b"})
	readUntil(t, bob, isType(TypeHistory))

	write(t, alice, Inbound{Type: TypeSend, Room: "a", Text: "secret"})
	write(t, bob, Inbound{Type: TypeSend, Room: "b", Text: "marker"})
	// Bob's next chat message must be his own marker, not Alice's secret.
	got := readUntil(t, bob, isType(TypeMessage))
	if got.Text != "marker" {
		t.Fatalf("bob saw a message from another room: %+v", got)
	}
}

func TestLateJoinerGetsHistory(t *testing.T) {
	srv := startServer(t)
	alice := dial(t, srv, "alice")
	write(t, alice, Inbound{Type: TypeJoin, Room: "general"})
	readUntil(t, alice, isType(TypeHistory))
	for i := 1; i <= 3; i++ {
		write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: fmt.Sprint("msg ", i)})
		readUntil(t, alice, isType(TypeMessage))
	}

	carol := dial(t, srv, "carol")
	write(t, carol, Inbound{Type: TypeJoin, Room: "general"})
	h := readUntil(t, carol, isType(TypeHistory))
	if len(h.Messages) != 3 || h.Messages[2].Text != "msg 3" {
		t.Fatalf("history = %+v", h.Messages)
	}
}

func TestSendWithoutJoinIsRejected(t *testing.T) {
	srv := startServer(t)
	alice := dial(t, srv, "alice")
	write(t, alice, Inbound{Type: TypeSend, Room: "general", Text: "hello?"})
	if got := readUntil(t, alice, isType(TypeError)); !strings.Contains(got.Text, "join") {
		t.Fatalf("got %+v", got)
	}
}

func TestInvalidNameIsRejected(t *testing.T) {
	srv := startServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?name=bad%20name"
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil || resp == nil || resp.StatusCode != 400 {
		t.Fatalf("want HTTP 400, got err=%v resp=%v", err, resp)
	}
}

// TestManyConcurrentClients is the concurrency test: 50 clients all send at
// once, and every client must receive every message. Run with -race to let
// Go's race detector check for unsafe shared memory.
func TestManyConcurrentClients(t *testing.T) {
	const n = 50
	srv := startServer(t)
	conns := make([]*websocket.Conn, n)
	for i := range conns {
		conns[i] = dial(t, srv, fmt.Sprint("user", i))
		write(t, conns[i], Inbound{Type: TypeJoin, Room: "load"})
		readUntil(t, conns[i], isType(TypeHistory))
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

	// Every client should see all n messages.
	errs := make(chan error, n)
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			seen := 0
			for seen < n {
				var out Outbound
				if err := wsjson.Read(ctx, c, &out); err != nil {
					errs <- fmt.Errorf("client %d saw %d/%d messages: %w", i, seen, n, err)
					return
				}
				if out.Type == TypeMessage {
					seen++
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
