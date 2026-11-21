package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The .sql files are compiled into the binary, so the schema always
// matches the code that uses it.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is an arbitrary number that names our advisory lock.
const migrationLockID = 727_001

// migrationsTable records which migration files have run.
const migrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    TEXT PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// unlockTimeout bounds how long releasing the migration lock may take.
const unlockTimeout = 5 * time.Second

// Migrate brings the database schema up to date by running every file in
// migrations/ that hasn't run yet, in name order. Each file runs in its own
// transaction: it either fully applies or not at all.
//
// It is safe to call from several servers starting at the same moment
// (step 5 runs three): a Postgres advisory lock makes them take turns, and
// the later ones find nothing left to do.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	unlock, err := lockMigrations(ctx, conn)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err = conn.Exec(ctx, migrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if err := applyMigration(ctx, conn, name); err != nil {
			return err
		}
	}
	return nil
}

// lockMigrations takes the migration lock and returns the function that
// releases it.
func lockMigrations(ctx context.Context, conn *pgxpool.Conn) (unlock func(), err error) {
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	return func() {
		// A fresh context: ctx may already be cancelled, and the lock must
		// be released anyway.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, unlockErr := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", migrationLockID); unlockErr != nil {
			// The lock belongs to this database session. Closing the
			// connection ends the session, which releases the lock; the
			// pool then opens a fresh connection when it needs one.
			slog.Warn("release migration lock; closing the connection instead", "err", unlockErr)
			_ = conn.Conn().Close(uctx)
		}
	}, nil
}

// applyMigration runs one migration file, unless it has run before.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, name string) error {
	version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
	var done bool
	err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done)
	if err != nil || done {
		return err
	}
	sql, err := migrationFiles.ReadFile(name)
	if err != nil {
		return err
	}
	// BeginFunc commits if the function returns nil and rolls back if it
	// returns an error, so a half-applied migration is impossible.
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// Exec without arguments uses Postgres' simple protocol, which
		// allows several statements in one call.
		if _, execErr := tx.Exec(ctx, string(sql)); execErr != nil {
			return fmt.Errorf("migration %s: %w", version, execErr)
		}
		_, execErr := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
		return execErr
	})
	if err != nil {
		return err
	}
	slog.Info("applied migration", "version", version)
	return nil
}
