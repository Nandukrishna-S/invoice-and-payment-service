// Package auth authenticates API keys and carries the caller's identity.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	keyPrefix = "sk_test_"
	// prefixLen is how much of a key is stored for display (keyPrefix + 4 hex chars).
	prefixLen = len(keyPrefix) + 4
)

// A key is keyPrefix plus 32 random bytes in hex.
var keyFormat = regexp.MustCompile(`^sk_test_[0-9a-f]{64}$`)

type APIKey struct {
	ID         uuid.UUID
	BusinessID uuid.UUID
	Prefix     string
	KeyHash    []byte
	RevokedAt  *time.Time
}

// GenerateKey returns a new random key. It is shown once and never stored.
func GenerateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return keyPrefix + hex.EncodeToString(b), nil
}

func ValidKeyFormat(key string) bool { return keyFormat.MatchString(key) }

// HashKey is a plain SHA-256: the key is 256 bits of randomness, so a slow
// salted hash would add nothing and would make lookup by key impossible.
func HashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func displayPrefix(key string) string { return key[:prefixLen] }
