package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/bcrypt"

	"github.com/MITRAKER/realtime-chat/internal/app"
	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
	"github.com/MITRAKER/realtime-chat/internal/postgres"
	"github.com/MITRAKER/realtime-chat/internal/postgres/postgrestest"
)

var secret = []byte("test-secret-that-is-at-least-32-bytes-long")

// newRoutes builds the whole app, like main() does, on top of the given
// stores. The hub stops when the test ends.
func newRoutes(t *testing.T, users auth.UserStore, messages chat.Store) http.Handler {
	t.Helper()
	tokens, err := auth.NewTokens(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(users, tokens, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	hub := chat.NewHub()
	go hub.Run(done)
	t.Cleanup(func() { close(done) })
	return app.Routes(app.Deps{
		Auth: svc, Tokens: tokens, Hub: hub, Messages: messages,
		Static: fstest.MapFS{"index.html": {Data: []byte("test page")}},
	})
}

// startServer runs the app on a local port. Cleanups run last-in,
// first-out, so the server stops before the hub does.
func startServer(t *testing.T, users auth.UserStore, messages chat.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newRoutes(t, users, messages))
	t.Cleanup(srv.Close)
	return srv
}

func signUp(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	body := `{"username":"` + name + `","password":"a good long password"}`
	res, err := http.Post(srv.URL+"/api/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var session auth.Session
	if err := json.NewDecoder(res.Body).Decode(&session); err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status %d, %v", name, res.StatusCode, err)
	}
	return session.Token
}

// connect opens the chat socket, logs in, and joins a room. It returns the
// connection and the history the server sent on joining.
func connect(t *testing.T, srv *httptest.Server, token, room string) (*websocket.Conn, chat.Outbound) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	must(t, wsjson.Write(ctx, conn, chat.Inbound{Type: chat.TypeAuth, Token: token}))
	readType(t, conn, chat.TypeReady)
	must(t, wsjson.Write(ctx, conn, chat.Inbound{Type: chat.TypeJoin, Room: room}))
	return conn, readType(t, conn, chat.TypeHistory)
}

func readType(t *testing.T, conn *websocket.Conn, typ string) chat.Outbound {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var m chat.Outbound
		must(t, wsjson.Read(ctx, conn, &m))
		if m.Type == typ {
			return m
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// chatFlow is the whole journey: sign up, connect, chat, and see history.
func chatFlow(t *testing.T, srv *httptest.Server) {
	alice, _ := connect(t, srv, signUp(t, srv, "alice"), "general")
	bob, _ := connect(t, srv, signUp(t, srv, "bob"), "general")

	ctx := context.Background()
	must(t, wsjson.Write(ctx, alice, chat.Inbound{Type: chat.TypeSend, Room: "general", Text: "hello bob"}))
	got := readType(t, bob, chat.TypeMessage)
	if got.User != "alice" || got.Text != "hello bob" {
		t.Fatalf("bob got %+v", got)
	}
}

func TestEndToEndInMemory(t *testing.T) {
	srv := startServer(t, auth.NewMemoryUserStore(), chat.NewMemoryStore(100))
	chatFlow(t, srv)

	res, err := http.Get(srv.URL + "/healthz")
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
}

// TestEndToEndPostgresSurvivesRestart checks the point of a database:
// after the server restarts, accounts and messages are still there.
func TestEndToEndPostgresSurvivesRestart(t *testing.T) {
	pool := postgrestest.NewPool(t)
	first := startServer(t, postgres.NewUserStore(pool), postgres.NewMessageStore(pool))
	chatFlow(t, first)
	first.Close()

	// A brand-new server process: new hub, empty memory, same database.
	second := startServer(t, postgres.NewUserStore(pool), postgres.NewMessageStore(pool))
	res, err := http.Post(second.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"ALICE","password":"a good long password"}`))
	must(t, err)
	var session auth.Session
	must(t, json.NewDecoder(res.Body).Decode(&session))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login after restart: %d", res.StatusCode)
	}
	_, history := connect(t, second, session.Token, "general")
	if len(history.Messages) != 1 || history.Messages[0].Text != "hello bob" || history.Messages[0].User != "alice" {
		t.Fatalf("history after restart = %+v", history.Messages)
	}
}
