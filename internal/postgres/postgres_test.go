package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/auth/authtest"
	"github.com/MITRAKER/realtime-chat/internal/chat"
	"github.com/MITRAKER/realtime-chat/internal/chat/storetest"
	"github.com/MITRAKER/realtime-chat/internal/postgres"
	"github.com/MITRAKER/realtime-chat/internal/postgres/postgrestest"
)

// These tests need a real PostgreSQL. Run them with, for example:
//
//	TEST_DATABASE_URL=postgres://chat:chat@localhost:5432/chat_test?sslmode=disable go test ./...

func TestUserStoreContract(t *testing.T) {
	authtest.Run(t, func(t *testing.T) auth.UserStore {
		return postgres.NewUserStore(postgrestest.NewPool(t))
	})
}

func TestMessageStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (chat.Store, func(string) auth.User) {
		pool := postgrestest.NewPool(t)
		users := postgres.NewUserStore(pool)
		return postgres.NewMessageStore(pool), func(name string) auth.User {
			u, err := users.CreateUser(context.Background(), name, "hash")
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
	})
}

func TestMigrateIsIdempotentAndSafeInParallel(t *testing.T) {
	pool := postgrestest.NewPool(t) // already migrated once
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- postgres.Migrate(context.Background(), pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations has %d rows, want 1", n)
	}
}

func TestMessagesNeedARealUser(t *testing.T) {
	pool := postgrestest.NewPool(t)
	store := postgres.NewMessageStore(pool)
	_, err := store.SaveMessage(context.Background(), "general", auth.User{ID: 999, Username: "ghost"}, "boo")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" { // foreign_key_violation
		t.Fatalf("err = %v, want a foreign key violation", err)
	}
}

func TestSQLInjectionIsJustText(t *testing.T) {
	pool := postgrestest.NewPool(t)
	users := postgres.NewUserStore(pool)
	ctx := context.Background()
	evil := "x'); DROP TABLE users; --"
	if _, err := users.CreateUser(ctx, evil, "hash"); err != nil {
		t.Fatal(err)
	}
	u, _, err := users.UserByUsername(ctx, evil)
	if err != nil || u.Username != evil {
		t.Fatalf("got %+v, %v", u, err)
	}
	store := postgres.NewMessageStore(pool)
	if _, err := store.SaveMessage(ctx, evil, u, evil); err != nil {
		t.Fatal(err)
	}
	msgs, _, err := store.History(ctx, evil, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Text != evil {
		t.Fatalf("history = %+v, %v", msgs, err)
	}
}

func TestHistoryUsesTheIndex(t *testing.T) {
	pool := postgrestest.NewPool(t)
	ctx := context.Background()
	users := postgres.NewUserStore(pool)
	u, err := users.CreateUser(ctx, "alice", "hash")
	if err != nil {
		t.Fatal(err)
	}
	// Enough rows in many rooms that a full scan would be the wrong plan.
	_, err = pool.Exec(ctx, `INSERT INTO messages (room, user_id, text)
		SELECT 'room-' || (g % 100), $1, 'msg ' || g FROM generate_series(1, 20000) g`, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE messages"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `EXPLAIN
		SELECT id FROM messages WHERE room = 'room-7' AND id < 9223372036854775807
		ORDER BY id DESC LIMIT 51`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if want := "messages_room_id_idx"; !strings.Contains(plan, want) {
		t.Fatalf("history query does not use %s:\n%s", want, plan)
	}
}
