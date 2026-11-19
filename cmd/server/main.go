// Command server runs the real-time chat server.
//
// Configuration comes from environment variables, so the same binary runs
// on a laptop, in CI and in production with nothing secret in the code:
//
//	ADDR          address to listen on (default ":8080")
//	DATABASE_URL  PostgreSQL URL; if empty, data is kept in memory only
//	JWT_SECRET    secret for signing login tokens, at least 32 bytes;
//	              if empty, a random one is made (logins reset on restart)
//	TOKEN_TTL     how long a login lasts (default "24h")
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MITRAKER/realtime-chat/internal/app"
	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
	"github.com/MITRAKER/realtime-chat/internal/postgres"
	"github.com/MITRAKER/realtime-chat/web"
)

// version is replaced at build time by the release tooling:
//
//	go build -ldflags "-X main.version=v0.3.0" ./cmd/server
var version = "dev"

// shutdownTimeout is how long requests already running, and chat
// connections saying goodbye, may take to finish once the server is told
// to stop.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(context.Background(), os.Getenv, nil); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// config is everything the server reads from its environment.
type config struct {
	addr        string
	databaseURL string
	jwtSecret   []byte
	tokenTTL    time.Duration
}

// loadConfig reads the configuration through getenv: os.Getenv in
// production, a map lookup in tests.
func loadConfig(getenv func(string) string) (config, error) {
	ttl, err := time.ParseDuration(cmp.Or(getenv("TOKEN_TTL"), "24h"))
	if err != nil {
		return config{}, fmt.Errorf("TOKEN_TTL: %w", err)
	}
	if ttl <= 0 {
		return config{}, fmt.Errorf("TOKEN_TTL must be positive, got %v", ttl)
	}
	secret, err := jwtSecret(getenv("JWT_SECRET"))
	if err != nil {
		return config{}, err
	}
	return config{
		addr:        cmp.Or(getenv("ADDR"), ":8080"),
		databaseURL: getenv("DATABASE_URL"),
		jwtSecret:   secret,
		tokenTTL:    ttl,
	}, nil
}

// jwtSecret checks JWT_SECRET's value, or makes a random secret for local
// development when it is empty.
func jwtSecret(fromEnv string) ([]byte, error) {
	if fromEnv != "" {
		if len(fromEnv) < auth.MinSecretLength {
			return nil, fmt.Errorf("JWT_SECRET must be at least %d characters", auth.MinSecretLength)
		}
		return []byte(fromEnv), nil
	}
	slog.Warn("JWT_SECRET not set: using a random secret, so everyone is logged out when the server restarts")
	b := make([]byte, auth.MinSecretLength)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// stores is where accounts and messages live.
type stores struct {
	users    auth.UserStore
	messages chat.Store
	ready    func(*http.Request) error // nil means always ready
	close    func()
}

// openStores uses PostgreSQL when databaseURL is set, and memory otherwise.
func openStores(ctx context.Context, databaseURL string) (stores, error) {
	if databaseURL == "" {
		slog.Warn("DATABASE_URL not set: accounts and messages are kept in memory and lost on restart")
		return stores{
			users:    auth.NewMemoryUserStore(),
			messages: chat.NewMemoryStore(500),
			close:    func() {},
		}, nil
	}
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return stores{}, err
	}
	if err = postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		return stores{}, fmt.Errorf("migrate: %w", err)
	}
	slog.Info("using PostgreSQL")
	return stores{
		users:    postgres.NewUserStore(pool),
		messages: postgres.NewMessageStore(pool),
		ready:    func(r *http.Request) error { return pool.Ping(r.Context()) },
		close:    pool.Close, // waits for queries still running to finish
	}, nil
}

// run builds the server from the environment and serves until ctx is
// cancelled or the process gets Ctrl+C (SIGINT) or SIGTERM.
//
// If listening isn't nil, run tells it the server's address once the
// server is ready for requests. Tests use it to learn which port the
// operating system picked for ADDR=127.0.0.1:0.
func run(ctx context.Context, getenv func(string) string, listening func(net.Addr)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting realtime-chat", "version", version)

	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	tokens, err := auth.NewTokens(cfg.jwtSecret, cfg.tokenTTL)
	if err != nil {
		return err
	}
	st, err := openStores(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	// Closed last (defers run in reverse), after the HTTP server has stopped.
	defer st.close()
	// Listen before the slow setup below, so a port that's already taken
	// fails at once. Connections that arrive early wait in the queue.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		return err
	}
	authService, err := auth.NewService(st.users, tokens, auth.DefaultBcryptCost)
	if err != nil {
		_ = ln.Close() // we never started serving, so nothing else will close it
		return err
	}

	// The hub stops as soon as we're told to shut down, and closes every
	// chat connection with "going away" (1001) so browsers can reconnect.
	// srv.Shutdown in serve can't do that part: it only waits for ordinary
	// HTTP requests, and WebSockets have left the HTTP server's hands. So
	// serve also waits for the hub (hub.Wait), or the program could end
	// before the goodbyes are sent.
	hub := chat.NewHub()
	go hub.Run(ctx.Done())

	routes := app.Routes(app.Deps{
		Auth: authService, Tokens: tokens, Hub: hub,
		Messages: st.messages, Static: web.Files, Ready: st.ready,
	})
	srv := app.NewHTTPServer(cfg.addr, routes, app.DefaultTimeouts())
	if listening != nil {
		listening(ln.Addr())
	}
	return serve(ctx, srv, ln, hub.Wait)
}

// serve answers requests on ln until ctx is cancelled, then shuts down
// gracefully: it stops accepting connections, lets requests already
// running (a login, say) finish, and then waits for whatever else must
// finish first (drain: the chat connections), all within shutdownTimeout.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, drain func(context.Context) error) error {
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", ln.Addr().String())
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err // Serve only returns early on a real error
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Serve returns http.ErrServerClosed once Shutdown has begun: expected.
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err := drain(shutdownCtx); err != nil {
		return fmt.Errorf("waiting for chat connections to close: %w", err)
	}
	return nil
}
