package webhooks

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	secretPrefix = "whsec_"
	// We generate 32-byte keys. Verification accepts 16 to 64 bytes, the range
	// Standard Webhooks allows, so secrets made elsewhere interoperate.
	secretBytes    = 32
	minSecretBytes = 16
	maxSecretBytes = 64
)

// NewSecret returns a fresh signing secret: whsec_ followed by the base64 of 32
// random bytes. As in the Standard Webhooks scheme, signatures are computed with
// the decoded bytes, so senders and receivers must decode it (see SecretKey).
func NewSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// SecretKey returns the raw HMAC key for a secret produced by NewSecret.
func SecretKey(secret string) ([]byte, error) {
	rest, ok := strings.CutPrefix(secret, secretPrefix)
	if !ok {
		return nil, errors.New("webhook secret must start with " + secretPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(rest)
	if err != nil || len(key) < minSecretBytes || len(key) > maxSecretBytes {
		return nil, errors.New("webhook secret is not a valid base64 key")
	}
	return key, nil
}
