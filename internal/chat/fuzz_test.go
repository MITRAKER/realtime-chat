package chat

// Fuzz tests: see the explanation at the top of internal/auth/fuzz_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// FuzzHandleFrame sends one arbitrary frame from a logged-in client, the
// way readLoop does once wsjson.Read has decoded it. These frames are the
// main thing strangers control. Whatever a frame says:
//   - handle must not crash, and must keep the connection open: bad input
//     gets an error message, not a disconnect;
//   - nothing invalid may reach the store (checkedStore watches);
//   - every reply must make sense (checkReply looks at each one).
func FuzzHandleFrame(f *testing.F) {
	for _, frame := range []string{
		`{"type":"send","room":"general","text":"hello"}`,
		`{"type":"send","room":" general ","text":"  two\nlines  "}`,
		`{"type":"send","room":"general","text":"nul\u0000"}`,
		`{"type":"send","room":"general","text":"` + strings.Repeat("é", maxTextLength/2+1) + `"}`,
		`{"type":"send","room":"elsewhere","text":"not joined"}`,
		`{"type":"join","room":"random","before":2}`,
		`{"type":"history","room":"general","before":3}`,
		`{"type":"history","room":"general","before":-1}`,
		`{"type":"leave","room":"general"}`,
		`{"type":"auth","token":"x"}`,
		`{"type":"dance"}`,
		`null`,
	} {
		f.Add([]byte(frame))
	}
	// Room names that look like "general" but aren't, and one with a space.
	// They're written with Go escapes because the characters themselves
	// would be invisible or misleading in this file (the bidichk and gosec
	// linters refuse some of them in source code: see "Trojan Source" in
	// LEARNING.md).
	for _, room := range []string{"gen\u200beral", "\u202elareneg", "general\u3164", "g\u0435neral", "game night"} {
		frame, err := json.Marshal(Inbound{Type: TypeJoin, Room: room})
		if err != nil {
			f.Fatal(err)
		}
		f.Add(frame)
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		var in Inbound
		if json.Unmarshal(frame, &in) != nil {
			return // wsjson.Read closes the connection (1007) before handle sees it
		}
		c := newFuzzClient(t)
		if !c.handle(t.Context(), in) {
			t.Fatalf("handle(%#v) closed the connection", in)
		}
		for _, m := range replies(t, c) {
			checkReply(t, in, m)
		}
	})
}

// newFuzzClient returns a client registered with a running hub that has
// joined "general", where three messages are waiting. That gives leave,
// history and send something to work on.
func newFuzzClient(t *testing.T) *Client {
	t.Helper()
	h := NewHub()
	done := make(chan struct{})
	go h.Run(done)
	t.Cleanup(func() {
		close(done)
		<-h.stopped
	})
	store := checkedStore{MemoryStore: NewMemoryStore(100), t: t}
	user := auth.User{ID: 1, Username: "fuzzer"}
	for _, text := range []string{"one", "two", "three"} {
		if _, err := store.SaveMessage(t.Context(), "general", user, text); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestClient(user.Username, sendBuffer)
	c.hub, c.store, c.user, c.limiter = h, store, user, rate.NewLimiter(rate.Inf, 0)
	if !h.submit(command{kind: cmdRegister, client: c}) || !c.handle(t.Context(), Inbound{Type: TypeJoin, Room: "general"}) {
		t.Fatal("could not join general")
	}
	replies(t, c) // the join announcement and the history page
	return c
}

// replies returns everything the hub has sent c so far. It asks the hub to
// send a marker, then reads up to it: the hub handles commands in the
// order they arrive, so the replies to earlier frames come first.
func replies(t *testing.T, c *Client) []Outbound {
	t.Helper()
	const marker = "end-of-replies"
	if !c.reply(Outbound{Type: marker}) {
		t.Fatal("the hub has stopped")
	}
	var got []Outbound
	// send holds at most sendBuffer messages, so the marker is among the
	// next sendBuffer+1 (JPL rule 2: every loop has a fixed upper bound).
	for range sendBuffer + 1 {
		select {
		case m, ok := <-c.send:
			if !ok {
				t.Fatal("the hub disconnected the client")
			}
			if m.Type == marker {
				return got
			}
			got = append(got, m)
		case <-time.After(5 * time.Second):
			t.Fatal("the hub stopped answering")
		}
	}
	t.Fatal("the marker never arrived")
	return nil
}

// checkReply checks one message the client got after sending the frame in.
// %#v and %q print strings in Go syntax, so control and format characters
// show up as escapes. %+q also escapes everything outside ASCII, which
// makes invisible letters such as U+3164 show up too.
func checkReply(t *testing.T, in Inbound, m Outbound) {
	t.Helper()
	if m.Type != TypeError && (m.Room != strings.TrimSpace(in.Room) || !goodRoom(m.Room)) {
		t.Errorf("for %#v the client got a %q reply in room %+q", in, m.Type, m.Room)
	}
	if problem := replyProblem(in, m); problem != "" {
		t.Errorf("for %#v the client got %s", in, problem)
	}
}

// replyProblem says what is wrong with the reply m to the frame in, or
// returns "" if nothing is.
func replyProblem(in Inbound, m Outbound) string {
	switch m.Type {
	case TypeError:
		if m.Text == "" {
			return "an error without a message"
		}
	case TypeMessage:
		if in.Type != TypeSend || m.Text != strings.TrimSpace(in.Text) || m.User != "fuzzer" {
			return fmt.Sprintf("the chat message %q from %q", m.Text, m.User)
		}
	case TypeSystem:
		if in.Type != TypeJoin || m.Text != "fuzzer joined" {
			return fmt.Sprintf("the system message %q", m.Text)
		}
	case TypeHistory:
		return historyProblem(in, m.Messages)
	default:
		return fmt.Sprintf("a message of type %q", m.Type)
	}
	return ""
}

// historyProblem checks a page of history: oldest first, no bigger than a
// page, and (when the client asked for older messages) only older ones.
func historyProblem(in Inbound, page []Outbound) string {
	if in.Type != TypeHistory && in.Type != TypeJoin || len(page) > historyPageSize {
		return fmt.Sprintf("a history page of %d messages", len(page))
	}
	for i, m := range page {
		if i > 0 && m.ID <= page[i-1].ID || in.Type == TypeHistory && in.Before > 0 && m.ID >= in.Before {
			return fmt.Sprintf("a history page with ids %v", ids(page))
		}
	}
	return ""
}

// checkedStore is a MemoryStore that fails the test when a message that
// must never be saved reaches it. Its rules are written out again here,
// separately from validRoomName and textProblem, on purpose: two
// independent descriptions of "valid" must agree.
type checkedStore struct {
	*MemoryStore
	t *testing.T
}

func (s checkedStore) SaveMessage(ctx context.Context, room string, from auth.User, text string) (Outbound, error) {
	if !goodRoom(room) || !goodText(text) {
		s.t.Errorf("saved a message that breaks the rules: room %q, text %q", room, text)
	}
	return s.MemoryStore.SaveMessage(ctx, room, from, text)
}

// goodRoom is the rule for room names: 1 to 64 ASCII letters, digits, '_'
// or '-'.
func goodRoom(room string) bool {
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	return len(room) >= 1 && len(room) <= 64 && strings.Trim(room, allowed) == ""
}

// goodText is the rule for chat messages: 1 to 2000 bytes of UTF-8 with
// no NUL and no spaces at either end.
func goodText(text string) bool {
	return text != "" && len(text) <= 2000 && text == strings.TrimSpace(text) &&
		utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

// FuzzMemoryStoreHistory compares MemoryStore.History, which finds its
// place with a binary search, with a model that simply looks at every
// message. This is differential testing: a fast, clever implementation
// and a slow, obviously right one must always agree. Index arithmetic like
// History's is exactly where off-by-one bugs live.
func FuzzMemoryStoreHistory(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 0, 0}, uint8(3), int64(5), 2)
	f.Add([]byte{0, 0, 0}, uint8(0), int64(0), 10)
	f.Add([]byte{1, 1, 1, 1}, uint8(2), int64(-7), -1)
	rooms := []string{"a", "b", "c"}
	f.Fuzz(func(t *testing.T, saves []byte, keep uint8, before int64, limit int) {
		store := NewMemoryStore(int(keep))
		var all []Outbound // every message ever saved, oldest first
		for _, b := range saves[:min(len(saves), 300)] {
			m, err := store.SaveMessage(t.Context(), rooms[int(b)%len(rooms)], auth.User{ID: 1, Username: "u"}, "x")
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, m)
		}
		for _, room := range rooms {
			got, more, err := store.History(t.Context(), room, before, limit)
			if err != nil {
				t.Fatal(err)
			}
			want, wantMore := historyModel(all, room, int(keep), before, limit)
			if !slices.Equal(ids(got), ids(want)) || more != wantMore {
				t.Fatalf("History(%q, before %d, limit %d) = %v, more %v; want %v, more %v",
					room, before, limit, ids(got), more, ids(want), wantMore)
			}
		}
	})
}

// historyModel is History done the slow, obvious way.
func historyModel(all []Outbound, room string, keep int, before int64, limit int) (page []Outbound, more bool) {
	var kept []Outbound
	for _, m := range all {
		if m.Room == room {
			kept = append(kept, m)
		}
	}
	kept = kept[len(kept)-min(len(kept), keep):] // the store forgets all but the newest `keep`
	var older []Outbound
	for _, m := range kept {
		if before <= 0 || m.ID < before {
			older = append(older, m)
		}
	}
	n := min(len(older), max(limit, 0)) // how many fit on the page
	return older[len(older)-n:], n < len(older)
}

func ids(msgs []Outbound) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}
