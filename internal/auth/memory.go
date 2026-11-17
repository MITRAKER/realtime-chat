package auth

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemoryUserStore keeps accounts in memory. Everything is lost when the
// server stops, so it is only for tests and for trying the app without a
// database.
type MemoryUserStore struct {
	mu     sync.Mutex
	nextID int64
	byName map[string]memoryUser // key: lowercase username
}

type memoryUser struct {
	user User
	hash string
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{byName: make(map[string]memoryUser)}
}

func (s *MemoryUserStore) CreateUser(_ context.Context, username, passwordHash string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(username)
	if _, taken := s.byName[key]; taken {
		return User{}, ErrUsernameTaken
	}
	s.nextID++
	u := User{ID: s.nextID, Username: username, CreatedAt: time.Now().UTC()}
	s.byName[key] = memoryUser{user: u, hash: passwordHash}
	return u, nil
}

func (s *MemoryUserStore) UserByUsername(_ context.Context, username string) (User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byName[strings.ToLower(username)]
	if !ok {
		return User{}, "", ErrNotFound
	}
	return m.user, m.hash, nil
}
