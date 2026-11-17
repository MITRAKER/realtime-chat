package chat

import (
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/coder/websocket"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// NewServer returns the HTTP routes: the WebSocket endpoint, a health check,
// and the static test page.
func NewServer(hub *Hub, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		// Step 1 has no login yet: the name comes from the URL.
		// Step 2 replaces this with a signed token (JWT).
		name := r.URL.Query().Get("name")
		if !validName.MatchString(name) {
			http.Error(w, "name must be 1-32 letters, digits, _ or -", http.StatusBadRequest)
			return
		}
		// Accept performs the HTTP -> WebSocket "upgrade" handshake.
		// By default it rejects cross-origin requests, which protects
		// against other websites opening sockets as your users.
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			slog.Debug("upgrade failed", "err", err)
			return
		}
		hub.Serve(r.Context(), conn, name)
	})
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}
