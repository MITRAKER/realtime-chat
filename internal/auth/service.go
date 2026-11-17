package auth

import (
	"context"
	"errors"
	"time"
)

// Service implements sign-up and login on top of a UserStore.
type Service struct {
	users     UserStore
	tokens    *Tokens
	cost      int
	dummyHash string
}

// NewService creates the account service. bcryptCost is usually
// DefaultBcryptCost; tests pass bcrypt.MinCost to run fast.
func NewService(users UserStore, tokens *Tokens, bcryptCost int) (*Service, error) {
	// A hash of a throwaway password, used to make logins for unknown
	// usernames take as long as logins for real ones (see Login).
	dummy, err := HashPassword("not-a-real-password", bcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{users: users, tokens: tokens, cost: bcryptCost, dummyHash: dummy}, nil
}

// Session is what a successful sign-up or login returns to the browser.
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      User      `json:"user"`
}

// Register creates a new account and logs it in.
func (s *Service) Register(ctx context.Context, username, password string) (Session, error) {
	if err := ValidateUsername(username); err != nil {
		return Session{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return Session{}, err
	}
	hash, err := HashPassword(password, s.cost)
	if err != nil {
		return Session{}, err
	}
	user, err := s.users.CreateUser(ctx, username, hash)
	if err != nil {
		return Session{}, err
	}
	return s.newSession(user)
}

// Login checks a username and password and returns a new session.
func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	user, hash, err := s.users.UserByUsername(ctx, username)
	if errors.Is(err, ErrNotFound) {
		// Do the same slow bcrypt work as for a real account. Otherwise
		// "no such user" would answer in 1 ms and "wrong password" in
		// 250 ms, and an attacker could time the responses to discover
		// which usernames exist.
		CheckPassword(s.dummyHash, password)
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, err
	}
	if !CheckPassword(hash, password) {
		return Session{}, ErrInvalidCredentials
	}
	return s.newSession(user)
}

func (s *Service) newSession(u User) (Session, error) {
	token, expires, err := s.tokens.Issue(u)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: expires, User: u}, nil
}
