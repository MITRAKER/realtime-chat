package app_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"github.com/MITRAKER/realtime-chat/internal/app"
	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
)

// startWithTimeouts runs the app behind app.NewHTTPServer, the same server
// main() uses, with the given timeouts.
func startWithTimeouts(t *testing.T, timeouts app.Timeouts) *httptest.Server {
	t.Helper()
	routes := newRoutes(t, auth.NewMemoryUserStore(), chat.NewMemoryStore(100))
	srv := httptest.NewUnstartedServer(routes)
	srv.Config = app.NewHTTPServer("", routes, timeouts)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// allTimeouts returns Timeouts with every field set to d.
func allTimeouts(d time.Duration) app.Timeouts {
	return app.Timeouts{ReadHeader: d, Read: d, Write: d, Idle: d}
}

func TestDefaultTimeoutsAreAllSet(t *testing.T) {
	d := app.DefaultTimeouts()
	for name, v := range map[string]time.Duration{
		"ReadHeader": d.ReadHeader, "Read": d.Read, "Write": d.Write, "Idle": d.Idle,
	} {
		if v <= 0 {
			t.Errorf("%s timeout is %v; zero means no timeout at all", name, v)
		}
	}
}

// TestServerTimeoutsDoNotCutWebSockets: the HTTP timeouts are short, but a
// chat connection must outlive all of them. Go's server clears its
// deadlines when a request is upgraded to a WebSocket; if it didn't, every
// chat would break after a few seconds.
func TestServerTimeoutsDoNotCutWebSockets(t *testing.T) {
	const short = 200 * time.Millisecond
	srv := startWithTimeouts(t, allTimeouts(short))
	alice, _ := connect(t, srv, signUp(t, srv, "alice"), "general")

	time.Sleep(5 * short) // well past every server timeout

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must(t, wsjson.Write(ctx, alice, chat.Inbound{Type: chat.TypeSend, Room: "general", Text: "still here"}))
	if got := readType(t, alice, chat.TypeMessage); got.Text != "still here" {
		t.Fatalf("got %+v", got)
	}
}

// TestSlowlorisClientIsDisconnected plays the Slowloris attack: start a
// request and never finish the headers. Without ReadHeaderTimeout the
// connection would stay open forever, and a few thousand of them would
// use up the server's connections and memory.
func TestSlowlorisClientIsDisconnected(t *testing.T) {
	timeouts := allTimeouts(time.Minute)
	timeouts.ReadHeader = 200 * time.Millisecond
	srv := startWithTimeouts(t, timeouts)

	conn := dialTCP(t, srv)
	_, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n") // no blank line: unfinished
	must(t, err)
	expectServerHangsUp(t, conn, bufio.NewReader(conn))
}

// TestIdleConnectionIsClosed: after answering, the server keeps the
// connection open for the next request (keep-alive), but not forever.
func TestIdleConnectionIsClosed(t *testing.T) {
	timeouts := allTimeouts(time.Minute)
	timeouts.Idle = 200 * time.Millisecond
	srv := startWithTimeouts(t, timeouts)

	conn := dialTCP(t, srv)
	_, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: test\r\n\r\n")
	must(t, err)
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	must(t, err)
	_, err = io.Copy(io.Discard, res.Body)
	must(t, err)
	must(t, res.Body.Close())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
	expectServerHangsUp(t, conn, r)
}

func dialTCP(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	must(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// expectServerHangsUp waits up to 5 seconds for the server to close the
// connection without sending anything more.
func expectServerHangsUp(t *testing.T, conn net.Conn, r *bufio.Reader) {
	t.Helper()
	must(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	b, err := r.ReadByte()
	switch {
	case err == nil:
		t.Fatalf("server sent %q instead of closing the connection", b)
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatal("server kept the connection open")
	case !errors.Is(err, io.EOF):
		t.Logf("connection ended with %v", err) // a reset is fine too
	}
}
