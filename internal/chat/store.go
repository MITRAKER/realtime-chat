package chat

import (
	"sync"
	"time"
)

// Store saves messages so people who join later can see what they missed.
// It is an interface so we can swap the in-memory version for PostgreSQL
// in a later step without touching the hub.
type Store interface {
	Save(room, user, text string, at time.Time) (Outbound, error)
	Recent(room string, limit int) ([]Outbound, error)
}

// MemoryStore keeps the last N messages per room in memory.
// The hub calls it from a single goroutine today, but we still guard it
// with a mutex because nothing stops other code from calling it concurrently.
type MemoryStore struct {
	mu     sync.Mutex
	nextID int64
	keep   int
	rooms  map[string][]Outbound
}

func NewMemoryStore(keepPerRoom int) *MemoryStore {
	return &MemoryStore{keep: keepPerRoom, rooms: make(map[string][]Outbound)}
}

func (s *MemoryStore) Save(room, user, text string, at time.Time) (Outbound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m := Outbound{Type: TypeMessage, Room: room, ID: s.nextID, User: user, Text: text, Time: at}
	msgs := append(s.rooms[room], m)
	if len(msgs) > s.keep {
		msgs = msgs[len(msgs)-s.keep:]
	}
	s.rooms[room] = msgs
	return m, nil
}

func (s *MemoryStore) Recent(room string, limit int) ([]Outbound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.rooms[room]
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	out := make([]Outbound, len(msgs))
	copy(out, msgs)
	return out, nil
}
