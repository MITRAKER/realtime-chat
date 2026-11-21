package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/time/rate"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

const (
	// defaultAuthTimeout is how long a new connection has to send its token.
	defaultAuthTimeout = 5 * time.Second
	// defaultFrameRate and defaultFrameBurst limit how fast one connection
	// may send frames: on average 10 per second, with short bursts of up to
	// 20. People don't type that fast; scripts flooding a room do.
	defaultFrameRate  = 10
	defaultFrameBurst = 20
	// StatusUnauthorized is the close code for a missing, bad or expired
	// token. Codes 4000-4999 are free for applications to use; 4401 mirrors
	// HTTP's 401 Unauthorized so it's easy to recognise in the browser.
	StatusUnauthorized websocket.StatusCode = 4401
)

var errNotAuth = errors.New("first message must be auth")

// Handler serves the /ws endpoint.
type Handler struct {
	hub         *Hub
	store       Store
	tokens      *auth.Tokens
	authTimeout time.Duration
	frameRate   rate.Limit
	frameBurst  int
}

// NewHandler returns the WebSocket handler for a hub, message store and
// token checker.
func NewHandler(hub *Hub, store Store, tokens *auth.Tokens) *Handler {
	return &Handler{
		hub: hub, store: store, tokens: tokens,
		authTimeout: defaultAuthTimeout,
		frameRate:   defaultFrameRate,
		frameBurst:  defaultFrameBurst,
	}
}

// ServeHTTP upgrades the request to a WebSocket, checks the login token,
// and then hands the connection to the hub.
//
// Browsers can't add an Authorization header to a WebSocket request, so
// the token arrives as the first message instead:
//
//	client: {"type": "auth", "token": "<jwt>"}
//	server: {"type": "ready", "userId": 1, "user": "alice"}
//
// If no valid token arrives within 5 seconds, the server closes the
// connection with code 4401.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Count this connection until it has completely finished, so that
	// Hub.Wait can wait for its goodbye at shutdown. Counting starts before
	// the upgrade: after it, the HTTP server no longer tracks the
	// connection, and srv.Shutdown won't wait for it.
	h.hub.conns.Add(1)
	defer h.hub.conns.Done()

	// Accept performs the HTTP -> WebSocket "upgrade" handshake.
	// By default it rejects cross-origin requests, which protects
	// against other websites opening sockets as your users.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Debug("upgrade failed", "err", err)
		return
	}
	// Whatever happens below, release the socket at the end. Closing an
	// already-closed connection returns an error we have no use for.
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxFrameSize)

	claims, err := h.authenticate(r.Context(), conn)
	if err != nil {
		_ = conn.Close(StatusUnauthorized, "unauthorized")
		return
	}
	user := claims.User()

	// A token only proves who you are until it expires, so close the
	// connection at that moment too. The browser then logs in again.
	expiry := time.AfterFunc(time.Until(claims.ExpiresAt), func() {
		_ = conn.Close(StatusUnauthorized, "token expired")
	})
	defer expiry.Stop()

	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	err = wsjson.Write(ctx, conn, Outbound{Type: TypeReady, UserID: user.ID, User: user.Username})
	cancel()
	if err != nil {
		return
	}
	limiter := rate.NewLimiter(h.frameRate, h.frameBurst)
	h.hub.serve(r.Context(), conn, h.store, user, limiter)
}

// authenticate waits for the first frame and checks its token.
func (h *Handler) authenticate(ctx context.Context, conn *websocket.Conn) (auth.Claims, error) {
	// When the time is up, close the connection with our own close code.
	// (A context deadline on the read would also stop it, but the library
	// then drops the connection without saying why.)
	timer := time.AfterFunc(h.authTimeout, func() {
		_ = conn.Close(StatusUnauthorized, "no login token received")
	})
	defer timer.Stop()

	var in Inbound
	if err := wsjson.Read(ctx, conn, &in); err != nil {
		return auth.Claims{}, err
	}
	if in.Type != TypeAuth {
		return auth.Claims{}, errNotAuth
	}
	return h.tokens.Parse(in.Token)
}
