// Package app connects all the pieces into one HTTP handler.
package app

import (
	"io/fs"
	"net/http"
	"time"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
)

// Deps is everything the routes need. main() builds it from the config;
// tests build it with in-memory stores.
type Deps struct {
	Auth     *auth.Service
	Tokens   *auth.Tokens
	Hub      *chat.Hub
	Messages chat.Store
	Static   fs.FS
	// Ready reports whether the server can serve traffic (for example, the
	// database answers). Nil means always ready.
	Ready func(*http.Request) error
}

// Routes returns the server's HTTP routes:
//
//	POST /api/register, POST /api/login, GET /api/me   accounts (auth package)
//	GET  /ws                                           the chat WebSocket
//	GET  /healthz                                      "is the process alive?"
//	GET  /readyz                                       "can it serve traffic?"
//	GET  /                                             the test page
func Routes(d Deps) http.Handler {
	mux := http.NewServeMux()
	d.Auth.Routes(mux)
	mux.Handle("GET /ws", chat.NewHandler(d.Hub, d.Messages, d.Tokens))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.Ready != nil {
			if err := d.Ready(r); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /", http.FileServerFS(d.Static))
	return mux
}

// Timeouts protect the server from clients that connect and then go quiet
// or trickle bytes slowly, each holding a connection (and its memory)
// forever. That attack is called Slowloris.
type Timeouts struct {
	ReadHeader time.Duration // to receive the request headers
	Read       time.Duration // to receive the whole request, body included
	Write      time.Duration // to send the whole response
	Idle       time.Duration // a kept-alive connection may sit unused
}

// DefaultTimeouts are generous for real browsers and short for attackers.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		ReadHeader: 5 * time.Second,
		Read:       15 * time.Second,
		Write:      15 * time.Second,
		Idle:       60 * time.Second,
	}
}

// NewHTTPServer configures the HTTP server. These timeouts don't cut
// WebSockets short: when a request is upgraded, Go's HTTP server hands
// the connection over and clears its deadlines, and the chat code manages
// its own (pings, write timeouts). TestServerTimeoutsDoNotCutWebSockets
// checks this.
func NewHTTPServer(addr string, h http.Handler, t Timeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: t.ReadHeader,
		ReadTimeout:       t.Read,
		WriteTimeout:      t.Write,
		IdleTimeout:       t.Idle,
	}
}
