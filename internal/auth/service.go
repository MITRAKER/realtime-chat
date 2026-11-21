package auth

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// ErrBusy means too many password checks are already running. The HTTP
// layer answers 503 Service Unavailable, and the browser can retry.
var ErrBusy = errors.New("server busy, try again in a moment")

// defaultHashWait is how long a sign-up or login waits for a free hashing
// slot before giving up with ErrBusy.
const defaultHashWait = 3 * time.Second

// Service implements sign-up and login on top of a UserStore.
type Service struct {
	users     UserStore
	tokens    *Tokens
	cost      int
	dummyHash string
	// hashSlots limits how many bcrypt computations run at once. Each one
	// keeps a CPU core busy for about 250 ms, so without a limit a burst of
	// login attempts (even with wrong passwords) could take every core and
	// freeze the chat for everyone. This is a semaphore built from a
	// buffered channel: sending takes a slot, receiving gives it back.
	hashSlots chan struct{}
	hashWait  time.Duration
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
	// At most half the cores (and at least one) may hash at once, so the
	// rest stay free for chatting.
	slots := max(1, runtime.NumCPU()/2)
	return &Service{
		users: users, tokens: tokens, cost: bcryptCost, dummyHash: dummy,
		hashSlots: make(chan struct{}, slots), hashWait: defaultHashWait,
	}, nil
}

// withHashSlot runs fn once a hashing slot is free, or returns ErrBusy if
// none frees up in time (or ctx.Err() if the request is cancelled).
func (s *Service) withHashSlot(ctx context.Context, fn func()) error {
	wait := time.NewTimer(s.hashWait)
	defer wait.Stop()
	select {
	case s.hashSlots <- struct{}{}:
	case <-wait.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.hashSlots }()
	fn()
	return nil
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
	var hash string
	var hashErr error
	if err := s.withHashSlot(ctx, func() { hash, hashErr = HashPassword(password, s.cost) }); err != nil {
		return Session{}, err
	}
	if hashErr != nil {
		return Session{}, hashErr
	}
	user, err := s.users.CreateUser(ctx, username, hash)
	if err != nil {
		return Session{}, err
	}
	return s.newSession(user)
}

// Login checks a username and password and returns a new session.
func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	// A name that could never have been registered can't be right, and
	// neither can a password bcrypt can't tell apart from others (see
	// hashable). Check before touching the database: odd input such as a
	// NUL character would otherwise make PostgreSQL return an error (a 500).
	if ValidateUsername(username) != nil || !hashable(password) {
		return Session{}, s.failLogin(ctx, password)
	}
	user, hash, err := s.users.UserByUsername(ctx, username)
	if errors.Is(err, ErrNotFound) {
		return Session{}, s.failLogin(ctx, password)
	}
	if err != nil {
		return Session{}, err
	}
	var ok bool
	if err := s.withHashSlot(ctx, func() { ok = CheckPassword(hash, password) }); err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, ErrInvalidCredentials
	}
	return s.newSession(user)
}

// failLogin does the same slow bcrypt work as checking a real account, then
// fails. Otherwise "no such user" would answer in 1 ms and "wrong password"
// in 250 ms, and an attacker could time the responses to discover which
// usernames exist.
func (s *Service) failLogin(ctx context.Context, password string) error {
	if err := s.withHashSlot(ctx, func() { CheckPassword(s.dummyHash, password) }); err != nil {
		return err
	}
	return ErrInvalidCredentials
}

func (s *Service) newSession(u User) (Session, error) {
	token, expires, err := s.tokens.Issue(u)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: expires, User: u}, nil
}
