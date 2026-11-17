package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/auth/authtest"
)

var secret = []byte("test-secret-that-is-at-least-32-bytes-long")

func newTokens(t *testing.T) *auth.Tokens {
	t.Helper()
	tokens, err := auth.NewTokens(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func newService(t *testing.T) *auth.Service {
	t.Helper()
	// bcrypt.MinCost keeps tests fast; production uses DefaultBcryptCost.
	s, err := auth.NewService(auth.NewMemoryUserStore(), newTokens(t), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMemoryUserStoreContract(t *testing.T) {
	authtest.Run(t, func(t *testing.T) auth.UserStore { return auth.NewMemoryUserStore() })
}

func TestNewTokensRejectsWeakSettings(t *testing.T) {
	if _, err := auth.NewTokens([]byte("too-short"), time.Hour); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := auth.NewTokens(secret, 0); err == nil {
		t.Error("zero lifetime accepted")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tokens := newTokens(t)
	tok, exp, err := tokens.Issue(auth.User{ID: 42, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tokens.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != 42 || claims.Username != "alice" || !claims.ExpiresAt.Equal(exp) {
		t.Fatalf("claims = %+v, expiry %v", claims, exp)
	}
	if until := time.Until(exp); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("expiry %v from now, want about 1h", until)
	}
}

func TestBadTokensAreRejected(t *testing.T) {
	tokens := newTokens(t)
	good, _, _ := tokens.Issue(auth.User{ID: 1, Username: "alice"})

	expiredIssuer := newTokens(t)
	expiredIssuer.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	expired, _, _ := expiredIssuer.Issue(auth.User{ID: 1, Username: "alice"})

	otherKey, _ := auth.NewTokens([]byte("another-secret-that-is-32-bytes-or-more"), time.Hour)
	wrongKey, _, _ := otherKey.Issue(auth.User{ID: 1, Username: "alice"})

	// Change the payload to claim to be user 2 but keep the old signature.
	parts := strings.Split(good, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), `"sub":"1"`, `"sub":"2"`, 1))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]

	// The classic "alg: none" attack: a token with no signature at all.
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "1", "name": "alice", "iss": "realtime-chat", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	// Same secret, different algorithm.
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"sub": "1", "name": "alice", "iss": "realtime-chat", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(secret)

	// Valid signature but no expiry: must still be refused.
	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "1", "name": "alice", "iss": "realtime-chat",
	}).SignedString(secret)

	// Valid signature but from someone else.
	wrongIssuer, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "1", "name": "alice", "iss": "someone-else", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(secret)

	badSubject, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "not-a-number", "name": "alice", "iss": "realtime-chat", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(secret)

	cases := map[string]string{
		"empty": "", "garbage": "abc.def.ghi", "expired": expired, "wrong key": wrongKey,
		"tampered": tampered, "alg none": none, "HS512": hs512, "no expiry": noExp,
		"wrong issuer": wrongIssuer, "bad subject": badSubject,
	}
	for name, tok := range cases {
		if _, err := tokens.Parse(tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}

func TestRegisterValidates(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	cases := []struct{ user, pass string }{
		{"ab", "long-enough-password"},                // name too short
		{"has space", "long-enough-password"},         // bad character
		{strings.Repeat("a", 33), "long-enough-pass"}, // name too long
		{"alice", "short"},                            // password too short
		{"alice", strings.Repeat("x", 73)},            // over bcrypt's 72-byte limit
		{"alice", "ééé"},                              // 6 bytes but only 3 characters
	}
	for _, c := range cases {
		var invalid *auth.ValidationError
		if _, err := s.Register(ctx, c.user, c.pass); !errors.As(err, &invalid) {
			t.Errorf("Register(%q, %q) err = %v, want ValidationError", c.user, c.pass, err)
		}
	}
}

func TestRegisterThenLogin(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	reg, err := s.Register(ctx, "Alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Token == "" || reg.User.Username != "Alice" || reg.User.ID == 0 {
		t.Fatalf("session = %+v", reg)
	}
	if _, err := s.Register(ctx, "alice", "another password"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("duplicate register err = %v", err)
	}
	login, err := s.Login(ctx, "ALICE", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if login.User.ID != reg.User.ID {
		t.Fatalf("login user = %+v, want id %d", login.User, reg.User.ID)
	}
	for _, c := range []struct{ user, pass string }{
		{"alice", "wrong password"},
		{"nobody", "correct horse battery"},
		{"alice", strings.Repeat("x", 100)},
	} {
		if _, err := s.Login(ctx, c.user, c.pass); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("Login(%q) err = %v, want ErrInvalidCredentials", c.user, err)
		}
	}
}

func TestPasswordIsHashedWithSalt(t *testing.T) {
	a, _ := auth.HashPassword("same password", bcrypt.MinCost)
	b, _ := auth.HashPassword("same password", bcrypt.MinCost)
	if a == b {
		t.Fatal("two hashes of the same password are identical: no salt?")
	}
	if strings.Contains(a, "same password") {
		t.Fatal("hash contains the password")
	}
	if !auth.CheckPassword(a, "same password") || auth.CheckPassword(a, "other") {
		t.Fatal("CheckPassword gives the wrong answer")
	}
}

// --- HTTP API ---

func newAPI(t *testing.T) (*httptest.Server, *auth.Service) {
	t.Helper()
	s := newService(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

func TestHTTPRegisterLoginMe(t *testing.T) {
	srv, _ := newAPI(t)

	code, body := post(t, srv.URL+"/api/register", `{"username":"bob","password":"hunter2hunter2"}`)
	if code != http.StatusCreated || body["token"] == "" {
		t.Fatalf("register: %d %v", code, body)
	}
	if _, leaked := body["user"].(map[string]any)["passwordHash"]; leaked {
		t.Fatal("password hash leaked in response")
	}

	code, body = post(t, srv.URL+"/api/register", `{"username":"BOB","password":"hunter2hunter2"}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate register: %d %v", code, body)
	}

	code, body = post(t, srv.URL+"/api/login", `{"username":"bob","password":"wrong-password"}`)
	if code != http.StatusUnauthorized || body["error"] != "invalid username or password" {
		t.Fatalf("bad login: %d %v", code, body)
	}

	code, body = post(t, srv.URL+"/api/login", `{"username":"bob","password":"hunter2hunter2"}`)
	if code != http.StatusOK {
		t.Fatalf("login: %d %v", code, body)
	}
	token := body["token"].(string)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me auth.User
	if err := json.NewDecoder(res.Body).Decode(&me); err != nil || res.StatusCode != http.StatusOK || me.Username != "bob" {
		t.Fatalf("me: %d %+v %v", res.StatusCode, me, err)
	}
}

func TestHTTPMeRequiresToken(t *testing.T) {
	srv, _ := newAPI(t)
	for _, header := range []string{"", "Bearer", "Bearer nonsense", "Basic dXNlcjpwYXNz"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", header, res.StatusCode)
		}
	}
}

func TestHTTPRejectsBadBodies(t *testing.T) {
	srv, _ := newAPI(t)
	huge := `{"username":"` + strings.Repeat("a", 10_000) + `","password":"x"}`
	for _, body := range []string{"", "not json", "[1,2,3]", huge} {
		code, out := post(t, srv.URL+"/api/register", body)
		if code != http.StatusBadRequest {
			t.Errorf("body %.20q: status %d %v, want 400", body, code, out)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc": "abc", "bearer abc": "abc", "Bearer  abc ": "abc",
		"": "", "Bearer": "", "Basic abc": "", "abc": "",
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)
		if got := auth.BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
