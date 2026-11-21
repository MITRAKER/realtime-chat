package chat

import (
	"cmp"
	"context"
	"math"
	"slices"
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

// NewMemoryStore returns an empty store that keeps the newest keepPerRoom
// messages of each room and forgets older ones.
func NewMemoryStore(keepPerRoom int) *MemoryStore {
	return &MemoryStore{keep: keepPerRoom, rooms: make(map[string][]Outbound), now: time.Now}
}

// SaveMessage stores a message, dropping the room's oldest one if the room
// is full.
func (s *MemoryStore) SaveMessage(_ context.Context, room string, from auth.User, text string) (Outbound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m := Outbound{
		Type: TypeMessage, Room: room, ID: s.nextID,
		UserID: from.ID, User: from.Username, Text: text, Time: s.now().UTC(),
	}
	s.rooms[room] = append(s.rooms[room], m)
	if extra := len(s.rooms[room]) - s.keep; extra > 0 {
		s.rooms[room] = s.rooms[room][extra:]
	}
	return m, nil
}

// History returns up to limit messages older than before, oldest first.
func (s *MemoryStore) History(_ context.Context, room string, before int64, limit int) ([]Outbound, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if before <= 0 {
		before = math.MaxInt64
	}
	msgs := s.rooms[room]
	// The slice is sorted by id, so binary search finds the first message
	// with id >= before in log(n) steps.
	end, _ := slices.BinarySearchFunc(msgs, before, func(m Outbound, id int64) int {
		return cmp.Compare(m.ID, id)
	})
	start := max(end-max(limit, 0), 0)
	// Copy, so the caller can't change our stored messages.
	return slices.Clone(msgs[start:end]), start > 0, nil
}
