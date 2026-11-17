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
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
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

func main() {
	if err := run(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := envOr("ADDR", ":8080")
	ttl, err := time.ParseDuration(envOr("TOKEN_TTL", "24h"))
	if err != nil {
		return fmt.Errorf("TOKEN_TTL: %w", err)
	}
	secret, err := jwtSecret()
	if err != nil {
		return err
	}
	tokens, err := auth.NewTokens(secret, ttl)
	if err != nil {
		return err
	}

	// Choose where data lives: PostgreSQL if configured, memory otherwise.
	var (
		users    auth.UserStore
		messages chat.Store
		ready    func(*http.Request) error
	)
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pool, err := postgres.Open(ctx, url)
		if err != nil {
			return err
		}
		// Closed last (defers run in reverse), after the HTTP server has
		// stopped. Close waits for queries still running to finish.
		defer pool.Close()
		if err := postgres.Migrate(ctx, pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		users = postgres.NewUserStore(pool)
		messages = postgres.NewMessageStore(pool)
		ready = func(r *http.Request) error { return pool.Ping(r.Context()) }
		slog.Info("using PostgreSQL")
	} else {
		slog.Warn("DATABASE_URL not set: accounts and messages are kept in memory and lost on restart")
		users = auth.NewMemoryUserStore()
		messages = chat.NewMemoryStore(500)
	}

	authService, err := auth.NewService(users, tokens, auth.DefaultBcryptCost)
	if err != nil {
		return err
	}
	// The hub stops as soon as we're told to shut down, and closes every
	// chat connection with "going away" (1001) so browsers can reconnect.
	// srv.Shutdown below can't do that part: it only waits for ordinary
	// HTTP requests, and WebSockets have left the HTTP server's hands.
	hub := chat.NewHub()
	go hub.Run(ctx.Done())

	routes := app.Routes(app.Deps{
		Auth: authService, Tokens: tokens, Hub: hub,
		Messages: messages, Static: web.Files, Ready: ready,
	})
	srv := app.NewHTTPServer(addr, routes, app.DefaultTimeouts())

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		// Stop accepting connections and let requests already running
		// (a login, say) finish, for up to 10 seconds.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	return nil
}

// jwtSecret reads JWT_SECRET, or makes a random one for local development.
func jwtSecret() ([]byte, error) {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		if len(s) < auth.MinSecretLength {
			return nil, fmt.Errorf("JWT_SECRET must be at least %d characters", auth.MinSecretLength)
		}
		return []byte(s), nil
	}
	slog.Warn("JWT_SECRET not set: using a random secret, so everyone is logged out when the server restarts")
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
