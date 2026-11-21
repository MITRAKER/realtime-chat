package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// request makes an HTTP request and returns the status code and body. It
// reads and closes the body itself, so no test can forget to.
func request(t *testing.T, method, url, body string) (status int, data []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	must(t, err)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	data, err = io.ReadAll(res.Body)
	must(t, errors.Join(err, res.Body.Close()))
	return res.StatusCode, data
}

// session posts a username and password to an account endpoint and
// returns the login token.
func session(t *testing.T, url, name string, wantStatus int) string {
	t.Helper()
	body := `{"username":"` + name + `","password":"a good long password"}`
	status, data := request(t, http.MethodPost, url, body)
	var s auth.Session
	if err := json.Unmarshal(data, &s); err != nil || status != wantStatus {
		t.Fatalf("POST %s as %s: status %d, %v", url, name, status, err)
	}
	return s.Token
}

func signUp(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	return session(t, srv.URL+"/api/register", name, http.StatusCreated)
}

// connect opens the chat socket, logs in, and joins "general". It returns
// the connection and the history the server sent on joining.
func connect(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, chat.Outbound) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket: "You never need to close resp.Body yourself."
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	must(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	must(t, wsjson.Write(ctx, conn, chat.Inbound{Type: chat.TypeAuth, Token: token}))
	readType(t, conn, chat.TypeReady)
	must(t, wsjson.Write(ctx, conn, chat.Inbound{Type: chat.TypeJoin, Room: "general"}))
	return conn, readType(t, conn, chat.TypeHistory)
}

func readType(t *testing.T, conn *websocket.Conn, typ string) chat.Outbound {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// jpl:loop returns when a frame of the wanted type arrives; a read
	// error (including the 5 s timeout) fails the test.
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
	t.Helper()
	alice, _ := connect(t, srv, signUp(t, srv, "alice"))
	bob, _ := connect(t, srv, signUp(t, srv, "bob"))

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

	if status, _ := request(t, http.MethodGet, srv.URL+"/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz = %d", status)
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
	token := session(t, second.URL+"/api/login", "ALICE", http.StatusOK)
	_, history := connect(t, second, token)
	if len(history.Messages) != 1 || history.Messages[0].Text != "hello bob" || history.Messages[0].User != "alice" {
		t.Fatalf("history after restart = %+v", history.Messages)
	}
}
