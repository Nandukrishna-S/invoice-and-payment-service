package webhooks

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	secretPrefix = "whsec_"
	secretBytes  = 32
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
	if err != nil || len(key) != secretBytes {
		return nil, errors.New("webhook secret is not a valid base64 key")
	}
	return key, nil
}
