// Package auth handles user accounts: checking usernames and passwords,
// hashing passwords, and issuing and checking the signed tokens (JWTs)
// that prove who a request comes from.
package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// User is a registered account. The password hash is deliberately not part
// of this struct, so it can't end up in a JSON response by accident.
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

var (
	// ErrUsernameTaken means another account already uses that name
	// (compared without caring about upper/lower case).
	ErrUsernameTaken = errors.New("username is already taken")
	// ErrInvalidCredentials is returned for a wrong username OR a wrong
	// password. We never say which one, so attackers can't use the login
	// form to find out which usernames exist.
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrNotFound is returned by a UserStore when no user matches.
	ErrNotFound = errors.New("user not found")
)

// UserStore is where accounts are saved. The postgres package implements it
// for real use, and MemoryUserStore implements it for tests and for running
// the server without a database.
type UserStore interface {
	// CreateUser saves a new account. It returns ErrUsernameTaken if the
	// name is already used, compared case-insensitively.
	CreateUser(ctx context.Context, username, passwordHash string) (User, error)
	// UserByUsername finds an account by name, case-insensitively, and
	// returns its password hash too. It returns ErrNotFound if there is none.
	UserByUsername(ctx context.Context, username string) (User, string, error)
}

// ValidationError explains why some input was rejected. Its message is
// safe to show to the user.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s %s", e.Field, e.Message) }

const (
	// MinPasswordLength is counted in characters.
	MinPasswordLength = 8
	// MaxPasswordBytes is a limit of the bcrypt algorithm: it only looks at
	// the first 72 bytes. We reject longer passwords instead of silently
	// ignoring the end of them.
	MaxPasswordBytes = 72
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// ValidateUsername checks the rules for a new username.
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return &ValidationError{Field: "username", Message: "must be 3-32 characters: letters, digits, _ or -"}
	}
	return nil
}

// ValidatePassword checks the rules for a new password.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return &ValidationError{Field: "password", Message: fmt.Sprintf("must be at least %d characters", MinPasswordLength)}
	}
	if len(password) > MaxPasswordBytes {
		return &ValidationError{Field: "password", Message: fmt.Sprintf("must be at most %d bytes", MaxPasswordBytes)}
	}
	if !hashable(password) {
		return &ValidationError{Field: "password", Message: "must not contain the NUL character"}
	}
	return nil
}

// hashable reports whether bcrypt can tell this password apart from every
// other password. bcrypt turns a password into a 72-byte key: it adds a
// NUL byte to the end, repeats the result until it fills 72 bytes, and
// ignores anything beyond that. So without these limits, different
// passwords make the same key and open the same account:
//
//   - a 72-byte password, and the same password with anything added;
//   - "password", and "password\x00password" (the key repeats either way).
//
// Without NUL bytes, and at most 72 bytes long, two different passwords
// always make different keys. FuzzLogin found the first case on its own.
//
// These are limits of the algorithm, not rules about strong passwords, so
// Login checks them too. (Login must not check the other rules: if the
// minimum length goes up one day, old accounts must still be able to log in.)
func hashable(password string) bool {
	return len(password) <= MaxPasswordBytes && !strings.ContainsRune(password, 0)
}
