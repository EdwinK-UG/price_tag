// Package auth holds password hashing and random-token helpers.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters follow the OWASP minimum (19 MiB, t=2, p=1). A login
// costs roughly 20-40 ms of one core.
const (
	argonTime    = 2
	argonMemKiB  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLen / MaxPasswordLen bound what we accept.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 128
)

// sem bounds concurrent hashing so a login flood cannot exhaust memory
// (each hash briefly needs ~19 MiB).
var sem = make(chan struct{}, 4)

var ErrWeakPassword = fmt.Errorf("password must be %d-%d characters", MinPasswordLen, MaxPasswordLen)

func CheckPasswordPolicy(pw string) error {
	if len(pw) < MinPasswordLen || len(pw) > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	if err := CheckPasswordPolicy(pw); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sem <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemKiB, argonThreads, argonKeyLen)
	<-sem
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against an encoded hash in constant time.
func VerifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	// Refuse absurd parameters from a tampered DB rather than OOM.
	if m > 256*1024 || t > 10 || p > 8 || m == 0 || t == 0 || p == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	sem <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-sem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash lets login take the same time whether or not the e-mail exists.
var dummyHash, _ = HashPassword("not-a-real-password")

func BurnVerify(pw string) { VerifyPassword(dummyHash, pw) }

// NewToken returns a random URL-safe token and its SHA-256 (what we store).
func NewToken(prefix string) (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// RandomPassword makes a readable one-time password (no look-alike characters).
func RandomPassword(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("no entropy"))
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
