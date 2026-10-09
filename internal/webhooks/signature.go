package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header names of the Standard Webhooks scheme.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// SignatureTolerance is how far a timestamp may be from the receiver's clock.
// Anything older is rejected, which is what bounds a replay of a captured request.
const SignatureTolerance = 5 * time.Minute

// Sign returns the webhook-signature header value for one delivery attempt:
// "v1," followed by the base64 of HMAC-SHA256(key, "{id}.{timestamp}.{body}"),
// where key is the decoded secret. The timestamp is the Unix time in seconds
// at which this attempt was signed, so every retry carries a fresh signature.
func Sign(secret, msgID string, timestamp int64, body []byte) (string, error) {
	key, err := SecretKey(secret)
	if err != nil {
		return "", err
	}
	return "v1," + base64.StdEncoding.EncodeToString(mac(key, msgID, timestamp, body)), nil
}

func mac(key []byte, msgID string, timestamp int64, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msgID))
	h.Write([]byte("."))
	h.Write([]byte(strconv.FormatInt(timestamp, 10)))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

var (
	ErrSignatureInvalid  = errors.New("no signature matches")
	ErrTimestampInvalid  = errors.New("webhook-timestamp is not a Unix time in seconds")
	ErrTimestampTooOld   = errors.New("webhook-timestamp is older than the tolerance")
	ErrTimestampInFuture = errors.New("webhook-timestamp is too far in the future")
)

// Verify checks a delivery the way a receiver must: the timestamp is within
// SignatureTolerance of now, and at least one of the space-separated "v1,<sig>"
// values in the header equals the signature computed from any of the secrets
// (several secrets or signatures let a key be rotated without downtime).
// Comparison is constant-time. body must be the raw bytes as received.
func Verify(secrets []string, msgID, timestampHeader, signatureHeader string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return ErrTimestampInvalid
	}
	switch age := now.Sub(time.Unix(ts, 0)); {
	case age > SignatureTolerance:
		return ErrTimestampTooOld
	case age < -SignatureTolerance:
		return ErrTimestampInFuture
	}

	var provided [][]byte
	for _, part := range strings.Fields(signatureHeader) {
		version, sig, found := strings.Cut(part, ",")
		if !found || version != "v1" {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(sig); err == nil {
			provided = append(provided, raw)
		}
	}
	for _, secret := range secrets {
		key, err := SecretKey(secret)
		if err != nil {
			return fmt.Errorf("receiver secret is malformed: %w", err)
		}
		want := mac(key, msgID, ts, body)
		for _, got := range provided {
			if hmac.Equal(want, got) {
				return nil
			}
		}
	}
	return ErrSignatureInvalid
}
