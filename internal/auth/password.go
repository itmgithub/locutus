// Package auth implements password hashing and the OIDC and SAMLv2 login flows.
package auth

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength is the minimum length of a local password.
const MinPasswordLength = 10

// HashPassword returns a bcrypt hash of password.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether password matches hash.
func CheckPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash is compared against when a user does not exist, so that login
// timing does not reveal which usernames exist.
var dummyHash, _ = HashPassword("locutus-timing-equaliser")

// CheckPasswordTiming wastes the same time as CheckPassword for unknown users.
func CheckPasswordTiming() { CheckPassword(dummyHash, "x") }

// RandomToken returns a URL-safe random string with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Identity is the user information returned by an external identity provider.
type Identity struct {
	Subject     string
	Username    string
	DisplayName string
	Email       string
}
