package auth_test

// Fuzz tests. Each one states a property that must hold for EVERY input,
// then lets a fuzzer search for an input that breaks it.
//
// `go test` runs only the seed inputs given to f.Add, as ordinary tests.
// `go test -run '^$' -fuzz '^FuzzLogin$' ./internal/auth` keeps inventing
// new inputs (mutating the seeds, guided by which code they reach) until
// you stop it or it finds a failure. A failing input is saved under
// testdata/fuzz/FuzzLogin/ and from then on runs as a seed, so the bug
// stays fixed. `make fuzz` and the nightly CI job fuzz every target.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/MITRAKER/realtime-chat/internal/auth"
)

// FuzzValidateUsername compares the regular expression in ValidateUsername
// with the rule written out by hand: 3 to 32 bytes, each an ASCII letter,
// a digit, '_' or '-'. Regular expressions are easy to get subtly wrong:
// in Python, `$` also matches just before a final newline, so "alice\n"
// would slip through. Go's `$` doesn't do that, and this test proves it.
func FuzzValidateUsername(f *testing.F) {
	for _, seed := range []string{"alice", "a_b-C9", "ab", "alice\n", "al ice", "ålice", strings.Repeat("a", 33)} {
		f.Add(seed)
	}
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	f.Fuzz(func(t *testing.T, name string) {
		want := len(name) >= 3 && len(name) <= 32 && strings.Trim(name, allowed) == ""
		err := auth.ValidateUsername(name)
		if got := err == nil; got != want {
			t.Fatalf("ValidateUsername(%q) accepted = %v, want %v", name, got, want)
		}
		var invalid *auth.ValidationError
		if err != nil && (!errors.As(err, &invalid) || invalid.Field != "username") {
			t.Fatalf("ValidateUsername(%q) = %v, want a ValidationError for the username", name, err)
		}
	})
}

// FuzzParseToken feeds the token parser arbitrary strings. Tokens arrive
// from anyone on the internet (an HTTP header, the WebSocket's first
// frame), so the parser must never crash. And it must never accept a
// token we didn't sign: the fuzzer can't forge an HMAC signature, so if
// anything it invents is accepted, a check is broken.
func FuzzParseToken(f *testing.F) {
	tokens := newTokens(f)
	issued, _, err := tokens.Issue(auth.User{ID: 7, Username: "alice"})
	if err != nil {
		f.Fatal(err)
	}
	// A token is header.payload.signature. The signature covers the text
	// "header.payload" exactly as written.
	dot := strings.LastIndex(issued, ".")
	signed, signature := issued[:dot], issued[dot+1:]
	ourSignature, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		f.Fatal(err)
	}
	header, payload, _ := strings.Cut(signed, ".")
	for _, seed := range []string{
		issued, "", ".", "..", "a.b.c", issued + "x", signed + ".",
		// The classic attack: the same claims with "alg": "none" and no signature.
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + payload + ".",
		// Our header with someone else's claims, and our signature.
		header + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"1","name":"admin"}`)) + "." + signature,
		respell(issued),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		claims, err := tokens.Parse(token)
		if err != nil {
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("Parse error %v is not ErrInvalidToken", err)
			}
			return
		}
		// Only the token we issued carries a valid signature. Its text
		// before the last dot must be exactly ours, and its signature the
		// same bytes, though they may be spelled differently (see respell).
		// Checking the claims instead wouldn't do: the "alg": "none" seed
		// carries our claims without our signature.
		dot := strings.LastIndex(token, ".")
		if dot < 0 || token[:dot] != signed {
			t.Fatalf("Parse(%q) accepted a token we never signed: %+v", token, claims)
		}
		theirs, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
		if err != nil || !bytes.Equal(theirs, ourSignature) {
			t.Fatalf("Parse(%q) accepted a signature that isn't ours: %+v", token, claims)
		}
	})
}

// respell writes the same token with its signature spelled differently.
// An HS256 signature is 32 bytes, 256 bits, and unpadded base64 spends 43
// characters of 6 bits on it, 258 bits: the last 2 bits mean nothing. The
// JWT library's decoder doesn't insist that they are 0 (Go's base64
// decoders only check that in Strict mode), so this token still has a
// valid signature. That's harmless: the signature's bytes are the same,
// and nobody without the secret can make them.
func respell(token string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, token[len(token)-1])
	return token[:len(token)-1] + string(alphabet[last^1]) // flip the very last bit
}

// FuzzTokenRoundTrip: a token made by Issue must Parse back to the same
// user, whatever the id and name.
func FuzzTokenRoundTrip(f *testing.F) {
	f.Add(int64(1), "alice")
	f.Add(int64(math.MaxInt64), "名前")
	f.Add(int64(42), `quote" backslash\ </script>`)
	tokens := newTokens(f)
	f.Fuzz(func(t *testing.T, id int64, name string) {
		// Parse refuses ids below 1 and empty names. And a JWT is JSON,
		// which can only carry valid UTF-8: encoding/json quietly
		// replaces bad bytes with U+FFFD, so such a name would come back
		// changed. Real names pass ValidateUsername (ASCII only) first.
		if id < 1 || name == "" || !utf8.ValidString(name) {
			return
		}
		token, _, err := tokens.Issue(auth.User{ID: id, Username: name})
		if err != nil {
			t.Fatal(err)
		}
		claims, err := tokens.Parse(token)
		if err != nil || claims.UserID != id || claims.Username != name {
			t.Fatalf("Issue then Parse for (%d, %q) gave %+v, %v", id, name, claims, err)
		}
	})
}

// FuzzLogin checks the most important property of a login: it works with
// the exact password, and with nothing else. The attempt is the real
// password plus a suffix, because "almost the password" is where password
// hashing schemes hide their surprises.
func FuzzLogin(f *testing.F) {
	f.Add("correct horse battery", "")
	f.Add("correct horse battery", "!")
	f.Add("correct horse battery", " ")
	// bcrypt ends the key with a NUL byte and repeats it to fill 72 bytes,
	// so these two are the same key. See hashable in user.go.
	f.Add("password", "\x00password")
	s := newService(f)
	var users atomic.Int64
	f.Fuzz(func(t *testing.T, password, suffix string) {
		name := fmt.Sprintf("user%d", users.Add(1))
		if _, err := s.Register(t.Context(), name, password); err != nil {
			var invalid *auth.ValidationError
			if errors.As(err, &invalid) {
				return // not a password anyone could have
			}
			t.Fatal(err)
		}
		_, err := s.Login(t.Context(), name, password+suffix)
		switch {
		case suffix == "" && err != nil:
			t.Fatalf("the right password %q was refused: %v", password, err)
		case suffix != "" && err == nil:
			t.Fatalf("the account with password %q also opens with %q", password, password+suffix)
		case suffix != "" && !errors.Is(err, auth.ErrInvalidCredentials):
			t.Fatalf("a wrong password gave %v, want ErrInvalidCredentials", err)
		}
	})
}

// FuzzAccountAPI posts arbitrary bodies to the sign-up and login endpoints.
// Whatever arrives, the answer must be a deliberate one, never a crash or
// a 500 (which means an error nobody planned for), and always JSON: a
// token on success, an error message otherwise.
func FuzzAccountAPI(f *testing.F) {
	for _, body := range []string{
		`{"username":"alice","password":"correct horse"}`,
		`{"username":"al\u0000ice","password":"correct horse"}`,
		`{"Username":"bob","PASSWORD":"correct horse"}`, // encoding/json ignores case in keys
		`{"username":"carol","password":"correct horse"} trailing`,
		`{"username":["x"]}`, `null`, `[]`, `"text"`, ``,
	} {
		f.Add(false, body)
		f.Add(true, body)
	}
	f.Fuzz(func(t *testing.T, login bool, body string) {
		mux := newAccountServer(t)
		path := "/api/register"
		if login {
			path = "/api/login"
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body)))
		switch rec.Code {
		case http.StatusOK, http.StatusCreated, http.StatusBadRequest, http.StatusUnauthorized, http.StatusConflict:
		default:
			t.Fatalf("POST %s %q: status %d", path, body, rec.Code)
		}
		var reply struct {
			Token string `json:"token"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("POST %s %q: reply %q is not JSON", path, body, rec.Body)
		}
		if success := rec.Code < 300; success && reply.Token == "" || !success && reply.Error == "" {
			t.Fatalf("POST %s %q: status %d with reply %q", path, body, rec.Code, rec.Body)
		}
	})
}

// newAccountServer returns the account API on a fresh store for each
// fuzz input, so every answer depends on that input alone and a failure
// replays exactly as it happened. "alice" already exists (password
// "correct horse"), so a single input can reach every outcome: a login
// that works, a wrong password, a name that's taken.
func newAccountServer(t *testing.T) *http.ServeMux {
	t.Helper()
	s, err := auth.NewService(pickyStore{auth.NewMemoryUserStore()}, newTokens(t), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Register(t.Context(), "alice", "correct horse"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	return mux
}

// pickyStore is a MemoryUserStore that fails the way PostgreSQL does when
// a username contains the NUL character: with an error nobody planned for.
// The API checks names before they reach the store; if a NUL ever got
// through, the answer would be a 500, and FuzzAccountAPI would see it.
type pickyStore struct{ *auth.MemoryUserStore }

var errNULByte = errors.New(`invalid byte sequence for encoding "UTF8": 0x00`)

func (s pickyStore) CreateUser(ctx context.Context, username, passwordHash string) (auth.User, error) {
	if strings.ContainsRune(username, 0) {
		return auth.User{}, errNULByte
	}
	return s.MemoryUserStore.CreateUser(ctx, username, passwordHash)
}

func (s pickyStore) UserByUsername(ctx context.Context, username string) (auth.User, string, error) {
	if strings.ContainsRune(username, 0) {
		return auth.User{}, "", errNULByte
	}
	return s.MemoryUserStore.UserByUsername(ctx, username)
}
