// Package postgrestest gives tests their own empty, migrated database
// schema, so tests never see each other's data and can run in parallel.
package postgrestest

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MITRAKER/realtime-chat/internal/postgres"
)

// NewPool returns a pool whose tables live in a fresh schema that is
// dropped when the test ends. It skips the test when TEST_DATABASE_URL is
// not set, so `go test ./...` still works without a database.
func NewPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		tb.Skip("set TEST_DATABASE_URL to run database tests")
	}
	ctx := context.Background()
	schema := "test_" + strings.ToLower(rand.Text()[:12])

	admin, err := postgres.Open(ctx, url)
	if err != nil {
		tb.Fatalf("connect: %v", err)
	}
	tb.Cleanup(admin.Close)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		tb.Fatalf("create schema: %v", err)
	}
	tb.Cleanup(func() {
		if _, dropErr := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); dropErr != nil {
			tb.Errorf("drop test schema %s: %v", schema, dropErr)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		tb.Fatal(err)
	}
	// Every connection in this pool looks in our schema first.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(pool.Close)
	if err = postgres.Migrate(ctx, pool); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	return pool
}
