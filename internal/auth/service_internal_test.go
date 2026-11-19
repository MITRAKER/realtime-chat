package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// failingStore fails the test if the service touches the database.
type failingStore struct{ t *testing.T }

func (f failingStore) CreateUser(context.Context, string, string) (User, error) {
	f.t.Error("CreateUser should not be called")
	return User{}, errors.New("unexpected call")
}

func (f failingStore) UserByUsername(context.Context, string) (User, string, error) {
	f.t.Error("UserByUsername should not be called")
	return User{}, "", errors.New("unexpected call")
}

func newTestService(t *testing.T, store UserStore) *Service {
	t.Helper()
	tokens, err := NewTokens([]byte("test-secret-that-is-at-least-32-bytes-long"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(store, tokens, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A malformed username can't belong to anyone, so it must fail like a wrong
// password without reaching the database (a NUL byte there is a 500).
func TestLoginRejectsImpossibleUsernamesBeforeTheDatabase(t *testing.T) {
	s := newTestService(t, failingStore{t})
	for _, name := range []string{"a\x00b", "", "x", "has space", strings.Repeat("a", 500)} {
		if _, err := s.Login(context.Background(), name, "some password"); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("Login(%q) err = %v, want ErrInvalidCredentials", name, err)
		}
	}
}

// When every hashing slot is taken, a login waits briefly and then reports
// ErrBusy (HTTP 503) instead of piling more work onto the CPU.
func TestLoginReportsBusyWhenHashSlotsAreFull(t *testing.T) {
	s := newTestService(t, NewMemoryUserStore())
	s.hashWait = 50 * time.Millisecond
	for range cap(s.hashSlots) {
		s.hashSlots <- struct{}{} // occupy every slot
	}
	if _, err := s.Login(context.Background(), "alice", "whatever-password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if _, err := s.Register(context.Background(), "alice", "whatever-password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("register err = %v, want ErrBusy", err)
	}

	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"alice","password":"whatever-password"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q; want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}

	<-s.hashSlots // free one slot: logins work again
	if _, err := s.Login(context.Background(), "nobody", "whatever-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("after freeing a slot: err = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginStopsWaitingWhenTheRequestIsCancelled(t *testing.T) {
	s := newTestService(t, NewMemoryUserStore())
	for range cap(s.hashSlots) {
		s.hashSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Login(ctx, "alice", "whatever-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
