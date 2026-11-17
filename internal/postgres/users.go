package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// uniqueViolation is the Postgres error code for "this would create a
// duplicate in a unique index".
const uniqueViolation = "23505"

// UserStore implements auth.UserStore.
type UserStore struct{ pool *pgxpool.Pool }

func NewUserStore(pool *pgxpool.Pool) *UserStore { return &UserStore{pool: pool} }

func (s *UserStore) CreateUser(ctx context.Context, username, passwordHash string) (auth.User, error) {
	var u auth.User
	// The values travel separately from the SQL text ($1, $2), so a
	// username like `'; DROP TABLE users; --` is just an odd name, never
	// SQL. That is how parameterized queries prevent SQL injection.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2)
		 RETURNING id, username, created_at`,
		username, passwordHash,
	).Scan(&u.ID, &u.Username, &u.CreatedAt)

	// We don't check "does this name exist?" before inserting: two sign-ups
	// at the same moment could both pass that check. The unique index is
	// the real guarantee, so we just try and translate its error.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return auth.User{}, auth.ErrUsernameTaken
	}
	return u, err
}

func (s *UserStore) UserByUsername(ctx context.Context, username string) (auth.User, string, error) {
	var u auth.User
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, created_at, password_hash FROM users
		 WHERE lower(username) = lower($1)`,
		username,
	).Scan(&u.ID, &u.Username, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, "", auth.ErrNotFound
	}
	return u, hash, err
}
