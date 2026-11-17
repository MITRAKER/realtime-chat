package auth

import "golang.org/x/crypto/bcrypt"

// DefaultBcryptCost is how much work hashing one password takes. Each +1
// doubles the time. 12 takes roughly a quarter of a second on a laptop:
// unnoticeable when you log in, but very expensive for an attacker who
// stole the database and wants to try billions of guesses.
const DefaultBcryptCost = 12

// HashPassword returns a bcrypt hash of the password. The hash includes a
// random salt and the cost, so two users with the same password still get
// different hashes, and we can raise the cost later without breaking old ones.
func HashPassword(password string, cost int) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches the stored hash.
// bcrypt compares in constant time, so the time taken doesn't reveal how
// many characters were right.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
