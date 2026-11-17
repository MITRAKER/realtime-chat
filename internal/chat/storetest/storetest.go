// Package storetest checks that a chat.Store behaves correctly.
//
// The same checks run against MemoryStore and against the PostgreSQL
// store. This is called a contract test: it makes sure the in-memory
// stand-in we use in other tests behaves like the real thing, so those
// tests can be trusted.
package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
)

// Factory returns a fresh, empty store and a function that creates a user
// the store can attribute messages to.
type Factory func(t *testing.T) (store chat.Store, newUser func(name string) auth.User)

// Run runs every contract check against stores made by newStore.
func Run(t *testing.T, newStore Factory) {
	ctx := context.Background()

	t.Run("SaveReturnsIDTimeAndAuthor", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		m, err := store.SaveMessage(ctx, "general", alice, "hello")
		if err != nil {
			t.Fatal(err)
		}
		if m.ID <= 0 || m.Time.IsZero() || m.Type != chat.TypeMessage {
			t.Fatalf("saved message missing fields: %+v", m)
		}
		if m.UserID != alice.ID || m.User != "alice" || m.Text != "hello" || m.Room != "general" {
			t.Fatalf("saved message has wrong content: %+v", m)
		}
	})

	t.Run("IDsIncrease", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		var last int64
		for i := range 5 {
			m, err := store.SaveMessage(ctx, "general", alice, fmt.Sprint(i))
			if err != nil {
				t.Fatal(err)
			}
			if m.ID <= last {
				t.Fatalf("id %d not greater than previous %d", m.ID, last)
			}
			last = m.ID
		}
	})

	t.Run("HistoryEmptyRoom", func(t *testing.T) {
		store, _ := newStore(t)
		msgs, more, err := store.History(ctx, "nobody-here", 0, 10)
		if err != nil || len(msgs) != 0 || more {
			t.Fatalf("got %v, more=%v, err=%v", msgs, more, err)
		}
	})

	t.Run("HistoryPagesOldestFirst", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		for i := 1; i <= 25; i++ {
			if _, err := store.SaveMessage(ctx, "general", alice, fmt.Sprint("msg ", i)); err != nil {
				t.Fatal(err)
			}
		}
		// Newest page: messages 16-25, and older ones exist.
		page, more, err := store.History(ctx, "general", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		wantTexts(t, page, 16, 25)
		if !more {
			t.Fatal("first page: want more=true")
		}
		// Next page goes below the oldest id we have.
		page, more, err = store.History(ctx, "general", page[0].ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		wantTexts(t, page, 6, 15)
		if !more {
			t.Fatal("second page: want more=true")
		}
		page, more, err = store.History(ctx, "general", page[0].ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		wantTexts(t, page, 1, 5)
		if more {
			t.Fatal("last page: want more=false")
		}
	})

	t.Run("HistoryExactlyOnePage", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		for i := 1; i <= 10; i++ {
			store.SaveMessage(ctx, "general", alice, fmt.Sprint("msg ", i))
		}
		page, more, err := store.History(ctx, "general", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		wantTexts(t, page, 1, 10)
		if more {
			t.Fatal("want more=false when the page holds everything")
		}
	})

	t.Run("HistoryKeepsRoomsApart", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		store.SaveMessage(ctx, "a", alice, "in a")
		store.SaveMessage(ctx, "b", alice, "in b")
		page, _, err := store.History(ctx, "a", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0].Text != "in a" || page[0].Room != "a" {
			t.Fatalf("room a history = %+v", page)
		}
	})

	t.Run("HistoryShowsAuthor", func(t *testing.T) {
		store, newUser := newStore(t)
		alice, bob := newUser("alice"), newUser("bob")
		store.SaveMessage(ctx, "general", alice, "hi")
		store.SaveMessage(ctx, "general", bob, "hey")
		page, _, err := store.History(ctx, "general", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 2 || page[0].User != "alice" || page[0].UserID != alice.ID || page[1].User != "bob" {
			t.Fatalf("history = %+v", page)
		}
	})

	t.Run("ConcurrentSaves", func(t *testing.T) {
		store, newUser := newStore(t)
		alice := newUser("alice")
		const n = 40
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := store.SaveMessage(ctx, "general", alice, fmt.Sprint(i)); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		page, more, err := store.History(ctx, "general", 0, n)
		if err != nil || len(page) != n || more {
			t.Fatalf("got %d messages (more=%v, err=%v), want %d", len(page), more, err, n)
		}
		seen := map[int64]bool{}
		for i, m := range page {
			if seen[m.ID] {
				t.Fatalf("duplicate id %d", m.ID)
			}
			seen[m.ID] = true
			if i > 0 && page[i-1].ID >= m.ID {
				t.Fatalf("history not sorted by id at %d", i)
			}
		}
	})
}

// wantTexts checks the page holds "msg from" ... "msg to", in that order.
func wantTexts(t *testing.T, page []chat.Outbound, from, to int) {
	t.Helper()
	if len(page) != to-from+1 {
		t.Fatalf("got %d messages, want %d (msg %d..%d)", len(page), to-from+1, from, to)
	}
	for i, m := range page {
		if want := fmt.Sprint("msg ", from+i); m.Text != want {
			t.Fatalf("message %d = %q, want %q", i, m.Text, want)
		}
	}
}
