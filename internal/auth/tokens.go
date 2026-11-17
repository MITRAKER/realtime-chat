package auth

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken covers every way a token can be bad: malformed, wrong
// signature, wrong algorithm, expired. Callers don't need to know which.
var ErrInvalidToken = errors.New("invalid or expired token")

// MinSecretLength is the shortest signing secret we accept. HS256 uses a
// 256-bit (32-byte) key, so a shorter secret is easier to brute-force.
const MinSecretLength = 32

const issuer = "realtime-chat"

// Claims is what a valid token tells us about the user.
type Claims struct {
	UserID    int64
	Username  string
	ExpiresAt time.Time
}

// User returns the account the claims describe.
func (c Claims) User() User { return User{ID: c.UserID, Username: c.Username} }

// Tokens issues and checks JSON Web Tokens signed with HMAC-SHA256 (HS256).
//
// A JWT is three base64 parts joined by dots: header.payload.signature.
// Anyone can decode the payload (it is NOT encrypted), but only someone
// with the secret can produce a valid signature. So the server can trust
// a token's contents without looking anything up in the database.
type Tokens struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewTokens creates a token issuer. ttl is how long each token stays valid.
func NewTokens(secret []byte, ttl time.Duration) (*Tokens, error) {
	if len(secret) < MinSecretLength {
		return nil, errors.New("JWT secret must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("token lifetime must be positive")
	}
	return &Tokens{secret: secret, ttl: ttl, now: time.Now}, nil
}

// jwtClaims is the JSON inside the token. "sub" (subject) is the standard
// claim for "who this token is about"; we add the username so the chat
// server can show names without a database lookup.
type jwtClaims struct {
	Username string `json:"name"`
	jwt.RegisteredClaims
}

// Issue creates a signed token for the user, and returns when it expires.
func (t *Tokens) Issue(u User) (string, time.Time, error) {
	now := t.now()
	expires := now.Add(t.ttl)
	claims := jwtClaims{
		Username: u.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(u.ID, 10),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	// JWT times are whole seconds, so report the expiry the token really has.
	return signed, claims.ExpiresAt.Time, nil
}

// Parse checks a token and returns its claims.
func (t *Tokens) Parse(token string) (Claims, error) {
	var c jwtClaims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		// Only accept the algorithm we sign with. Without this, an attacker
		// could send a token with "alg": "none" (no signature at all) or try
		// to confuse the server about which kind of key to use.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || id <= 0 || c.Username == "" {
		return Claims{}, ErrInvalidToken
	}
	return Claims{UserID: id, Username: c.Username, ExpiresAt: c.ExpiresAt.Time}, nil
}

// SetClock replaces the clock used to stamp and check tokens. Tests use it
// to make tokens that are already expired.
func (t *Tokens) SetClock(now func() time.Time) { t.now = now }
