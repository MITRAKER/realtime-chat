package chat

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// Store saves messages so people who join later can see what they missed.
// The postgres package implements it for real use; MemoryStore is for
// tests and for running without a database.
type Store interface {
	// SaveMessage stores a message and returns it with its id and time.
	// Ids only ever increase, so they also give the order of messages.
	SaveMessage(ctx context.Context, room string, from auth.User, text string) (Outbound, error)
	// History returns up to limit messages from room whose id is less than
	// before (0 means "the newest"), oldest first. more reports whether
	// even older messages exist.
	History(ctx context.Context, room string, before int64, limit int) (msgs []Outbound, more bool, err error)
}

// MemoryStore keeps the last N messages per room in memory.
type MemoryStore struct {
	mu     sync.Mutex
	nextID int64
	keep   int
	rooms  map[string][]Outbound // each slice is sorted by id
	now    func() time.Time
}

func NewMemoryStore(keepPerRoom int) *MemoryStore {
	return &MemoryStore{keep: keepPerRoom, rooms: make(map[string][]Outbound), now: time.Now}
}

func (s *MemoryStore) SaveMessage(_ context.Context, room string, from auth.User, text string) (Outbound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m := Outbound{
		Type: TypeMessage, Room: room, ID: s.nextID,
		UserID: from.ID, User: from.Username, Text: text, Time: s.now().UTC(),
	}
	msgs := append(s.rooms[room], m)
	if len(msgs) > s.keep {
		msgs = msgs[len(msgs)-s.keep:]
	}
	s.rooms[room] = msgs
	return m, nil
}

func (s *MemoryStore) History(_ context.Context, room string, before int64, limit int) ([]Outbound, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if before <= 0 {
		before = math.MaxInt64
	}
	msgs := s.rooms[room]
	end := len(msgs) // first index with id >= before
	for end > 0 && msgs[end-1].ID >= before {
		end--
	}
	start := max(end-limit, 0)
	out := make([]Outbound, end-start)
	copy(out, msgs[start:end])
	return out, start > 0, nil
}
