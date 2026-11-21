// Package storetest checks that a chat.Store behaves correctly.
//
// The same checks run against MemoryStore and against the PostgreSQL
// store. This is called a contract test: it makes sure the in-memory
// stand-in we use in other tests behaves like the real thing, so those
// tests can be trusted.
package storetest

import (
	"context"
	"errors"
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
	t.Helper()
	checks := []struct {
		name  string
		check func(*testing.T, Factory)
	}{
		{"SaveReturnsIDTimeAndAuthor", saveReturnsIDTimeAndAuthor},
		{"IDsIncrease", idsIncrease},
		{"HistoryEmptyRoom", historyEmptyRoom},
		{"HistoryPagesOldestFirst", historyPagesOldestFirst},
		{"HistoryExactlyOnePage", historyExactlyOnePage},
		{"HistoryKeepsRoomsApart", historyKeepsRoomsApart},
		{"HistoryShowsAuthor", historyShowsAuthor},
		{"ConcurrentSaves", concurrentSaves},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.check(t, newStore) })
	}
}

func saveReturnsIDTimeAndAuthor(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	m := mustSave(t, store, "general", alice, "hello")
	if m.ID <= 0 || m.Time.IsZero() || m.Type != chat.TypeMessage {
		t.Fatalf("saved message missing fields: %+v", m)
	}
	if m.UserID != alice.ID || m.User != "alice" || m.Text != "hello" || m.Room != "general" {
		t.Fatalf("saved message has wrong content: %+v", m)
	}
}

func idsIncrease(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	var last int64
	for i := range 5 {
		m := mustSave(t, store, "general", alice, fmt.Sprint(i))
		if m.ID <= last {
			t.Fatalf("id %d not greater than previous %d", m.ID, last)
		}
		last = m.ID
	}
}

func historyEmptyRoom(t *testing.T, newStore Factory) {
	store, _ := newStore(t)
	msgs, more, err := store.History(context.Background(), "nobody-here", 0, 10)
	if err != nil || len(msgs) != 0 || more {
		t.Fatalf("got %v, more=%v, err=%v", msgs, more, err)
	}
}

func historyPagesOldestFirst(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	for i := 1; i <= 25; i++ {
		mustSave(t, store, "general", alice, fmt.Sprint("msg ", i))
	}
	// Newest page: messages 16-25, and older ones exist. Each next page
	// asks for messages below the oldest id it already has.
	pages := []struct {
		from, to int
		more     bool
	}{{16, 25, true}, {6, 15, true}, {1, 5, false}}
	var before int64
	for _, want := range pages {
		page := mustHistory(t, store, "general", before, 10, want.more)
		wantTexts(t, page, want.from, want.to)
		before = page[0].ID
	}
}

func historyExactlyOnePage(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	for i := 1; i <= 10; i++ {
		mustSave(t, store, "general", alice, fmt.Sprint("msg ", i))
	}
	// more=false: the page holds everything there is.
	wantTexts(t, mustHistory(t, store, "general", 0, 10, false), 1, 10)
}

func historyKeepsRoomsApart(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	mustSave(t, store, "a", alice, "in a")
	mustSave(t, store, "b", alice, "in b")
	page := mustHistory(t, store, "a", 0, 10, false)
	if len(page) != 1 || page[0].Text != "in a" || page[0].Room != "a" {
		t.Fatalf("room a history = %+v", page)
	}
}

func historyShowsAuthor(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice, bob := newUser("alice"), newUser("bob")
	mustSave(t, store, "general", alice, "hi")
	mustSave(t, store, "general", bob, "hey")
	page := mustHistory(t, store, "general", 0, 10, false)
	if len(page) != 2 || page[0].User != "alice" || page[0].UserID != alice.ID || page[1].User != "bob" {
		t.Fatalf("history = %+v", page)
	}
}

func concurrentSaves(t *testing.T, newStore Factory) {
	store, newUser := newStore(t)
	alice := newUser("alice")
	const n = 40
	// One error slot per goroutine: each writes only its own slot, so no
	// lock is needed, and wg.Wait makes the writes visible to us.
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = store.SaveMessage(context.Background(), "general", alice, fmt.Sprint(i))
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	page := mustHistory(t, store, "general", 0, n, false)
	if len(page) != n {
		t.Fatalf("got %d messages, want %d", len(page), n)
	}
	for i := 1; i < len(page); i++ {
		if page[i-1].ID >= page[i].ID {
			t.Fatalf("history not sorted by unique ids at %d: %d then %d", i, page[i-1].ID, page[i].ID)
		}
	}
}

func mustSave(t *testing.T, store chat.Store, room string, from auth.User, text string) chat.Outbound {
	t.Helper()
	m, err := store.SaveMessage(context.Background(), room, from, text)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// mustHistory loads a page of history and checks its "more" flag.
func mustHistory(t *testing.T, store chat.Store, room string, before int64, limit int, wantMore bool) []chat.Outbound {
	t.Helper()
	page, more, err := store.History(context.Background(), room, before, limit)
	if err != nil {
		t.Fatal(err)
	}
	if more != wantMore {
		t.Fatalf("history(%q, before=%d): more=%v, want %v", room, before, more, wantMore)
	}
	return page
}

// wantTexts checks the page holds "msg from" ... "msg to", in that order.
func wantTexts(t *testing.T, page []chat.Outbound, from, to int) {
	t.Helper()
	if len(page) != to-from+1 {
		t.Fatalf("got %d messages, want %d (msg %d..%d)", len(page), to-from+1, from, to)
	}
	for i := range page {
		if want := fmt.Sprint("msg ", from+i); page[i].Text != want {
			t.Fatalf("message %d = %q, want %q", i, page[i].Text, want)
		}
	}
}
