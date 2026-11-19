// Package authtest checks that an auth.UserStore behaves correctly. Like
// chat/storetest, the same contract runs against the in-memory store and
// the PostgreSQL store.
package authtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// Factory returns a fresh, empty store.
type Factory func(t *testing.T) auth.UserStore

// Run runs every contract check against stores made by newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	checks := []struct {
		name  string
		check func(*testing.T, auth.UserStore)
	}{
		{"CreateAndFind", createAndFind},
		{"UnknownUser", unknownUser},
		{"NamesAreUniqueIgnoringCase", namesAreUniqueIgnoringCase},
		{"ConcurrentSignupsForSameName", concurrentSignupsForSameName},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.check(t, newStore(t)) })
	}
}

func createAndFind(t *testing.T, s auth.UserStore) {
	ctx := context.Background()
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
}

func unknownUser(t *testing.T, s auth.UserStore) {
	if _, _, err := s.UserByUsername(context.Background(), "ghost"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func namesAreUniqueIgnoringCase(t *testing.T, s auth.UserStore) {
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "bob", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "BOB", "h"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
}

// concurrentSignupsForSameName: ten people try to grab the same name at
// the same moment. Exactly one must win, whatever the timing.
func concurrentSignupsForSameName(t *testing.T, s auth.UserStore) {
	const n = 10
	errs := make([]error, n) // each goroutine writes only its own slot
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.CreateUser(context.Background(), "carol", fmt.Sprint("hash", i))
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, auth.ErrUsernameTaken):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d sign-ups won, want exactly 1", wins)
	}
}
