package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The .sql files are compiled into the binary, so the schema always
// matches the code that uses it.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is an arbitrary number that names our advisory lock.
const migrationLockID = 727_001

// Migrate brings the database schema up to date by running every file in
// migrations/ that hasn't run yet, in name order. Each file runs in its own
// transaction: it either fully applies or not at all.
//
// It is safe to call from several servers starting at the same moment
// (we will run three in step 4): a Postgres advisory lock makes them take
// turns, and the later ones find nothing left to do.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	// Session-level advisory locks are also released if the connection
	// dies, so a crashed server can't leave the lock stuck.
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var done bool
		err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		// Exec without arguments uses Postgres' simple protocol, which
		// allows several statements in one call.
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		slog.Info("applied migration", "version", version)
	}
	return nil
}
