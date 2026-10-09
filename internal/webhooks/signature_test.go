package webhooks

import (
	"strconv"
	"testing"
	"time"
)

// A fixed example: the signature below was computed independently with openssl
// (see the report) and also matches the published Standard Webhooks example.
const (
	vecSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vecID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vecTS     = int64(1614265330)
	vecBody   = `{"test": 2432232314}`
	vecSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestSignMatchesTheStandardWebhooksVector(t *testing.T) {
	got, err := Sign(vecSecret, vecID, vecTS, []byte(vecBody))
	if err != nil {
		t.Fatal(err)
	}
	if got != vecSig {
		t.Fatalf("signature = %s, want %s", got, vecSig)
	}
}

func TestSignDependsOnEveryInput(t *testing.T) {
	base, _ := Sign(vecSecret, vecID, vecTS, []byte(vecBody))
	other, _ := NewSecret()
	variants := map[string]func() (string, error){
		"id":        func() (string, error) { return Sign(vecSecret, vecID+"x", vecTS, []byte(vecBody)) },
		"timestamp": func() (string, error) { return Sign(vecSecret, vecID, vecTS+1, []byte(vecBody)) },
		"body":      func() (string, error) { return Sign(vecSecret, vecID, vecTS, []byte(vecBody+" ")) },
		"secret":    func() (string, error) { return Sign(other, vecID, vecTS, []byte(vecBody)) },
	}
	for name, f := range variants {
		got, err := f()
		if err != nil || got == base {
			t.Errorf("changing the %s must change the signature (err %v)", name, err)
		}
	}
	// "a.b" + "c" and "a" + "b.c" must not collide: the separators are part of the signed text.
	x, _ := Sign(vecSecret, "a.b", 1, []byte("c"))
	y, _ := Sign(vecSecret, "a", 1, []byte("b.c"))
	if x == y {
		t.Error("the fields are not unambiguously delimited")
	}
	if _, err := Sign("not-a-secret", vecID, vecTS, nil); err == nil {
		t.Error("a malformed secret must be rejected, not used")
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(vecTS, 0)
	ts := strconv.FormatInt(vecTS, 10)
	other, _ := NewSecret()
	otherSig, _ := Sign(other, vecID, vecTS, []byte(vecBody))

	tests := []struct {
		name    string
		secrets []string
		id      string
		ts      string
		sig     string
		body    string
		now     time.Time
		want    error
	}{
		{"valid", []string{vecSecret}, vecID, ts, vecSig, vecBody, now, nil},
		{"valid a little late", []string{vecSecret}, vecID, ts, vecSig, vecBody, now.Add(SignatureTolerance - time.Second), nil},
		{"valid a little early", []string{vecSecret}, vecID, ts, vecSig, vecBody, now.Add(-SignatureTolerance + time.Second), nil},
		{"tampered body", []string{vecSecret}, vecID, ts, vecSig, vecBody + "x", now, ErrSignatureInvalid},
		{"tampered id", []string{vecSecret}, vecID + "x", ts, vecSig, vecBody, now, ErrSignatureInvalid},
		{"tampered timestamp", []string{vecSecret}, vecID, strconv.FormatInt(vecTS+1, 10), vecSig, vecBody, now, ErrSignatureInvalid},
		{"wrong secret", []string{other}, vecID, ts, vecSig, vecBody, now, ErrSignatureInvalid},
		{"signature made with another secret", []string{vecSecret}, vecID, ts, otherSig, vecBody, now, ErrSignatureInvalid},
		{"replayed after the tolerance", []string{vecSecret}, vecID, ts, vecSig, vecBody, now.Add(SignatureTolerance + time.Second), ErrTimestampTooOld},
		{"timestamp from the future", []string{vecSecret}, vecID, ts, vecSig, vecBody, now.Add(-SignatureTolerance - time.Second), ErrTimestampInFuture},
		{"timestamp not a number", []string{vecSecret}, vecID, "yesterday", vecSig, vecBody, now, ErrTimestampInvalid},
		{"timestamp missing", []string{vecSecret}, vecID, "", vecSig, vecBody, now, ErrTimestampInvalid},
		{"no signature", []string{vecSecret}, vecID, ts, "", vecBody, now, ErrSignatureInvalid},
		{"garbage signature", []string{vecSecret}, vecID, ts, "v1,!!!", vecBody, now, ErrSignatureInvalid},
		{"unknown version only", []string{vecSecret}, vecID, ts, "v2," + vecSig[3:], vecBody, now, ErrSignatureInvalid},
		{"rotation: second signature matches", []string{vecSecret}, vecID, ts, otherSig + " " + vecSig, vecBody, now, nil},
		{"rotation: second secret matches", []string{other, vecSecret}, vecID, ts, vecSig, vecBody, now, nil},
		{"no secrets configured", nil, vecID, ts, vecSig, vecBody, now, ErrSignatureInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(tt.secrets, tt.id, tt.ts, tt.sig, []byte(tt.body), tt.now); err != tt.want {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	if err := Verify([]string{"bad"}, vecID, ts, vecSig, []byte(vecBody), now); err == nil || err == ErrSignatureInvalid {
		t.Errorf("a malformed receiver secret is a configuration error, not a bad signature: %v", err)
	}
}
