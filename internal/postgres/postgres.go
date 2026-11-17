// Package postgres stores users and chat messages in PostgreSQL.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to the database and checks that it answers.
//
// A pool keeps several connections open and lends them out, because
// opening a new connection for every query is slow. It also caps how many
// queries run at once (pool_max_conns in the URL, default 4 or the number
// of CPUs). When all connections are busy, new queries wait their turn,
// which protects the database from being overwhelmed.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
