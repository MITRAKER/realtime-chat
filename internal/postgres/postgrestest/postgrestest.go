// Package postgrestest gives tests their own empty, migrated database
// schema, so tests never see each other's data and can run in parallel.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MITRAKER/realtime-chat/internal/postgres"
)

// NewPool returns a pool whose tables live in a fresh schema that is
// dropped when the test ends. It skips the test when TEST_DATABASE_URL is
// not set, so `go test ./...` still works without a database.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run database tests")
	}
	ctx := context.Background()

	suffix := make([]byte, 6)
	rand.Read(suffix)
	schema := "test_" + hex.EncodeToString(suffix)

	admin, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	// Every connection in this pool looks in our schema first.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
