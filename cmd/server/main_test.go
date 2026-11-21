package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
)

const goodSecret = "a-test-secret-that-is-long-enough-1234"

// env returns a getenv function that reads from a map, so tests never
// depend on (or change) the real environment.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":8080" || cfg.databaseURL != "" || cfg.tokenTTL != 24*time.Hour {
		t.Fatalf("defaults = %+v", cfg)
	}
	if len(cfg.jwtSecret) < 32 {
		t.Fatalf("random secret has %d bytes, want at least 32", len(cfg.jwtSecret))
	}
	again, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again.jwtSecret, cfg.jwtSecret) {
		t.Fatal("two random secrets are identical")
	}
}

func TestLoadConfigReadsTheEnvironment(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"ADDR": "127.0.0.1:9000", "DATABASE_URL": "postgres://db", "JWT_SECRET": goodSecret, "TOKEN_TTL": "90m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:9000" || cfg.databaseURL != "postgres://db" ||
		string(cfg.jwtSecret) != goodSecret || cfg.tokenTTL != 90*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		key, value string
	}{
		{"TOKEN_TTL", "a while"},
		{"TOKEN_TTL", "-5m"},
		{"JWT_SECRET", "short"},
	}
	for _, c := range cases {
		_, err := loadConfig(env(map[string]string{c.key: c.value}))
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s=%q: err = %v, want an error naming %s", c.key, c.value, err, c.key)
		}
	}
}

func TestRunFailsFastOnBadSetup(t *testing.T) {
	cases := map[string]map[string]string{
		"bad config":         {"TOKEN_TTL": "soon"},
		"no database there":  {"DATABASE_URL": "postgres://nobody@127.0.0.1:1/chat?connect_timeout=2"},
		"address not usable": {"ADDR": "127.0.0.1:99999"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context(), env(vars), nil); err == nil {
				t.Fatal("run succeeded, want an error")
			}
		})
	}
}

// TestRunStartsAndStops runs the real server (in memory mode) on a port the
// operating system picks, then stops it the way Ctrl+C would: by
// cancelling its context. Stopping must not cut chat connections off: run
// may only return once each one has been told why it is closing (1001,
// "going away"). Otherwise the program could exit first, and browsers
// would see the connection simply drop.
func TestRunStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	vars := map[string]string{"ADDR": "127.0.0.1:0", "JWT_SECRET": goodSecret}
	go func() { done <- run(ctx, env(vars), func(addr net.Addr) { ready <- addr.String() }) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run stopped before it was ready: %v", err)
	case <-time.After(time.Minute): // bcrypt setup is slow under -race
		t.Fatal("run never got ready")
	}
	if got := get(ctx, "http://"+addr+"/healthz"); got != "ok" {
		t.Fatalf("/healthz answered %q, want ok", got)
	}
	conn := dialChat(t, addr)

	cancel()
	// The chat connection isn't reading yet, so its goodbye can't finish,
	// and run must still be waiting for it.
	select {
	case err := <-done:
		t.Fatalf("run returned (%v) before the chat connection was told why it closes", err)
	case <-time.After(300 * time.Millisecond):
	}
	if code := readCloseCode(t, conn); code != websocket.StatusGoingAway {
		t.Fatalf("close code = %v, want %v", code, websocket.StatusGoingAway)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want a clean stop", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("run did not stop after the chat connection closed")
	}
}

// dialChat opens a chat connection to the server at addr and logs in with
// a token signed with goodSecret, the server's JWT_SECRET.
func dialChat(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	tokens, err := auth.NewTokens([]byte(goodSecret), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(auth.User{ID: 1, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	//nolint:bodyclose // coder/websocket: "You never need to close resp.Body yourself."
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	var hello chat.Outbound
	if err = wsjson.Write(ctx, conn, chat.Inbound{Type: chat.TypeAuth, Token: token}); err == nil {
		err = wsjson.Read(ctx, conn, &hello)
	}
	if err != nil || hello.Type != chat.TypeReady {
		t.Fatalf("logging in to the chat: got %+v, %v", hello, err)
	}
	return conn
}

// readCloseCode reads until the connection closes and returns the close
// code the server sent (-1 if it sent none).
func readCloseCode(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// jpl:loop returns on the first read error: the close, or the timeout.
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// TestServeAnswersThenShutsDownGracefully: a request that is still running
// when shutdown starts gets to finish.
func TestServeAnswersThenShutsDownGracefully(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}, ln, nothingToDrain)
	}()

	reply := make(chan string, 1)
	go func() { reply <- get(context.WithoutCancel(ctx), "http://"+ln.Addr().String()+"/slow") }()
	<-started
	cancel() // shut down while /slow is still working

	if got := <-reply; got != "finished" {
		t.Fatalf("slow request got %q, want it to finish", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve returned %v", err)
	}
}

func TestServeReportsListenerErrors(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = ln.Close(); err != nil { // a closed listener makes Serve fail at once
		t.Fatal(err)
	}
	err = serve(t.Context(), &http.Server{ReadHeaderTimeout: time.Second}, ln, nothingToDrain)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve returned %v, want the listener's error", err)
	}
}

// TestServeReportsWhatCouldNotFinish: after the HTTP server has stopped,
// serve waits for drain with the time that is left, and reports it if
// drain doesn't finish.
func TestServeReportsWhatCouldNotFinish(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // shut down straight away
	stuck := func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
			return errors.New("drain got no time to finish")
		}
		return context.DeadlineExceeded // as if the time ran out
	}
	err = serve(ctx, &http.Server{ReadHeaderTimeout: time.Second}, ln, stuck)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("serve returned %v, want drain's error", err)
	}
}

func nothingToDrain(context.Context) error { return nil }

// testClient gives up on a request after a while, so a server that stops
// answering fails the test instead of hanging it.
var testClient = &http.Client{Timeout: 10 * time.Second}

// get fetches a URL and returns the body, or the error text. It runs in
// its own goroutine, so it reports errors instead of failing the test.
func get(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err.Error()
	}
	res, err := testClient.Do(req)
	if err != nil {
		return err.Error()
	}
	body, err := io.ReadAll(res.Body)
	if err = errors.Join(err, res.Body.Close()); err != nil {
		return err.Error()
	}
	return string(body)
}
