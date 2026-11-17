// Package authtest checks that an auth.UserStore behaves correctly. Like
// chat/storetest, the same contract runs against the in-memory store and
// the PostgreSQL store.
package authtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// Run runs every contract check against stores made by newStore.
func Run(t *testing.T, newStore func(t *testing.T) auth.UserStore) {
	ctx := context.Background()

	t.Run("CreateAndFind", func(t *testing.T) {
		s := newStore(t)
		u, err := s.CreateUser(ctx, "Alice", "hash-1")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID <= 0 || u.Username != "Alice" || u.CreatedAt.IsZero() {
			t.Fatalf("created user = %+v", u)
		}
		// Lookups ignore case but return the name as it was registered.
		got, hash, err := s.UserByUsername(ctx, "aLiCe")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != u.ID || got.Username != "Alice" || hash != "hash-1" {
			t.Fatalf("found %+v with hash %q", got, hash)
		}
	})

	t.Run("UnknownUser", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.UserByUsername(ctx, "ghost"); !errors.Is(err, auth.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("NamesAreUniqueIgnoringCase", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateUser(ctx, "bob", "h"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateUser(ctx, "BOB", "h"); !errors.Is(err, auth.ErrUsernameTaken) {
			t.Fatalf("err = %v, want ErrUsernameTaken", err)
		}
	})

	t.Run("ConcurrentSignupsForSameName", func(t *testing.T) {
		// Ten people try to grab the same name at the same moment.
		// Exactly one must win, whatever the timing.
		s := newStore(t)
		var wins, taken atomic.Int32
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.CreateUser(ctx, "carol", fmt.Sprint("hash", i))
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, auth.ErrUsernameTaken):
					taken.Add(1)
				default:
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 || taken.Load() != 9 {
			t.Fatalf("wins=%d taken=%d, want 1 and 9", wins.Load(), taken.Load())
		}
	})
}
