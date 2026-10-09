package webhookreceiver

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"invoice-and-payment-service/internal/webhooks"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

const sampleBody = `{"id":"evt_1","type":"invoice.paid","created_at":"2026-10-09T12:00:00Z","data":{"object":{"id":"inv-123","status":"paid","customer_id":"c1"}}}`

func signedRequest(t *testing.T, path, secret, id string, ts time.Time, body string) *http.Request {
	t.Helper()
	sig, err := webhooks.Sign(secret, id, ts.Unix(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(webhooks.HeaderID, id)
	req.Header.Set(webhooks.HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set(webhooks.HeaderSignature, sig)
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newSecret(t *testing.T) string {
	t.Helper()
	s, err := webhooks.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAVerifiedDeliveryIsAcceptedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	secret := newSecret(t)
	h := New(secret).Routes()

	rec := serve(h, signedRequest(t, "/hook", secret, "evt_1", time.Now(), sampleBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	out := logs.String()
	for _, want := range []string{`"signature_verified":true`, `"webhook_id":"evt_1"`, `"event_type":"invoice.paid"`, `"invoice_id":"inv-123"`, `"invoice_status":"paid"`, `"redelivery":false`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s: %s", want, out)
		}
	}
	// The secret, the signature and the body stay out of the log.
	for _, banned := range []string{secret, strings.TrimPrefix(secret, "whsec_"), "customer_id", "c1"} {
		if strings.Contains(out, banned) {
			t.Errorf("log contains %q", banned)
		}
	}
}

func TestRejectsWhatDoesNotVerifyAndSaysWhy(t *testing.T) {
	secret := newSecret(t)
	now := time.Now()
	tests := []struct {
		name   string
		mutate func(*http.Request)
		reason string
	}{
		{"tampered body", func(r *http.Request) {
			r.Body = httptest.NewRequest("POST", "/", strings.NewReader(sampleBody+" ")).Body
		}, "no signature matches"},
		{"wrong signature", func(r *http.Request) { r.Header.Set(webhooks.HeaderSignature, "v1,AAAA") }, "no signature matches"},
		{"missing signature", func(r *http.Request) { r.Header.Del(webhooks.HeaderSignature) }, "no signature matches"},
		{"another webhook-id", func(r *http.Request) { r.Header.Set(webhooks.HeaderID, "evt_other") }, "no signature matches"},
		{"stale timestamp", func(r *http.Request) {
			r.Header.Set(webhooks.HeaderTimestamp, strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10))
		}, "older than the tolerance"},
		{"timestamp from the future", func(r *http.Request) {
			r.Header.Set(webhooks.HeaderTimestamp, strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10))
		}, "too far in the future"},
		{"missing timestamp", func(r *http.Request) { r.Header.Del(webhooks.HeaderTimestamp) }, "not a Unix time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			h := New(secret).Routes()
			req := signedRequest(t, "/hook", secret, "evt_1", now, sampleBody)
			tt.mutate(req)
			rec := serve(h, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401 so the sender keeps retrying", rec.Code)
			}
			out := logs.String()
			if !strings.Contains(out, `"signature_verified":false`) || !strings.Contains(out, tt.reason) || !strings.Contains(out, `"level":"WARN"`) {
				t.Fatalf("expected a warning saying %q: %s", tt.reason, out)
			}
		})
	}
}

func TestWithoutASecretNothingVerifiesAndTheLogSaysHowToFixIt(t *testing.T) {
	logs := captureLogs(t)
	h := New().Routes()
	rec := serve(h, signedRequest(t, "/hook", newSecret(t), "evt_1", time.Now(), sampleBody))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(logs.String(), "POST /secrets") {
		t.Fatalf("%d %s", rec.Code, logs.String())
	}
}

func TestRedeliveriesAreFlaggedButStillAccepted(t *testing.T) {
	logs := captureLogs(t)
	secret := newSecret(t)
	h := New(secret).Routes()
	for i := 0; i < 2; i++ {
		if rec := serve(h, signedRequest(t, "/hook", secret, "evt_dup", time.Now(), sampleBody)); rec.Code != 200 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	out := logs.String()
	if strings.Count(out, `"redelivery":false`) != 1 || strings.Count(out, `"redelivery":true`) != 1 {
		t.Fatalf("exactly the second delivery should be flagged: %s", out)
	}
}

func TestSeenIDsAreBounded(t *testing.T) {
	r := New(newSecret(t))
	h := r.Routes()
	for i := 0; i < maxSeen+50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader("{}"))
		req.Header.Set(webhooks.HeaderID, fmt.Sprintf("evt_%d", i))
		serve(h, req)
	}
	if len(r.seen) > maxSeen || len(r.order) > maxSeen {
		t.Fatalf("remembering %d ids; the memory must be bounded at %d", len(r.seen), maxSeen)
	}
}

func TestStatusRouteAnswersWithTheRequestedCodeAfterVerifying(t *testing.T) {
	logs := captureLogs(t)
	secret := newSecret(t)
	h := New(secret).Routes()
	for _, code := range []int{200, 204, 301, 410, 429, 500, 503} {
		rec := serve(h, signedRequest(t, fmt.Sprintf("/hook/status/%d", code), secret, "evt_s", time.Now(), sampleBody))
		if rec.Code != code {
			t.Errorf("/hook/status/%d answered %d", code, rec.Code)
		}
	}
	if !strings.Contains(logs.String(), `"path":"/hook/status/503"`) || !strings.Contains(logs.String(), `"signature_verified":true`) {
		t.Fatal("the delivery must still be verified and logged")
	}
	for _, bad := range []string{"abc", "99", "600", "-1"} {
		if rec := serve(h, signedRequest(t, "/hook/status/"+bad, secret, "evt_s", time.Now(), sampleBody)); rec.Code != http.StatusBadRequest {
			t.Errorf("/hook/status/%s answered %d, want 400", bad, rec.Code)
		}
	}
}

func TestSlowRouteAnswersLate(t *testing.T) {
	old := slowDelay
	slowDelay = 150 * time.Millisecond
	defer func() { slowDelay = old }()
	secret := newSecret(t)
	h := New(secret).Routes()
	start := time.Now()
	rec := serve(h, signedRequest(t, "/hook/slow", secret, "evt_slow", time.Now(), sampleBody))
	if rec.Code != 200 || time.Since(start) < 140*time.Millisecond {
		t.Fatalf("%d after %v", rec.Code, time.Since(start))
	}
}

func TestSecretsCanBeAddedAtRuntimeAndRotated(t *testing.T) {
	r := New()
	h := r.Routes()
	first, second := newSecret(t), newSecret(t)
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/secrets", strings.NewReader(body))
		return serve(h, req).Code
	}
	if post(fmt.Sprintf(`{"secret":%q}`, first)) != http.StatusCreated {
		t.Fatal("adding a secret should be 201")
	}
	if post(fmt.Sprintf(`{"secret":%q}`, first)) != http.StatusOK {
		t.Fatal("adding it again should be a harmless 200")
	}
	if len(r.secrets) != 1 {
		t.Fatalf("%d secrets", len(r.secrets))
	}
	for name, body := range map[string]string{
		"malformed secret": `{"secret":"nope"}`,
		"missing secret":   `{}`,
		"unknown field":    `{"secret":"` + second + `","x":1}`,
		"not json":         `secret`,
	} {
		if code := post(body); code < 400 {
			t.Errorf("%s: got %d", name, code)
		}
	}
	if len(r.secrets) != 1 {
		t.Fatal("rejected requests must add nothing")
	}

	// Both the old and the new secret verify during a rotation.
	post(fmt.Sprintf(`{"secret":%q}`, second))
	for _, s := range []string{first, second} {
		if rec := serve(h, signedRequest(t, "/hook", s, "evt_r", time.Now(), sampleBody)); rec.Code != 200 {
			t.Errorf("a delivery signed with a known secret was rejected: %d", rec.Code)
		}
	}
	if rec := serve(h, signedRequest(t, "/hook", newSecret(t), "evt_r", time.Now(), sampleBody)); rec.Code != 401 {
		t.Errorf("an unknown secret must not verify: %d", rec.Code)
	}
}

func TestSeededSecretWorksAndHealthz(t *testing.T) {
	secret := newSecret(t)
	h := New(secret, "").Routes() // an empty seed is ignored
	if rec := serve(h, signedRequest(t, "/hook", secret, "evt_1", time.Now(), sampleBody)); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/healthz", nil)); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body)
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	secret := newSecret(t)
	h := New(secret).Routes()
	big := `{"x":"` + strings.Repeat("a", maxBodyBytes+10) + `"}`
	if rec := serve(h, signedRequest(t, "/hook", secret, "evt_big", time.Now(), big)); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestConcurrentDeliveriesAreSafe(t *testing.T) {
	secret := newSecret(t)
	h := New(secret).Routes()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := serve(h, signedRequest(t, "/hook", secret, fmt.Sprintf("evt_%d", i%10), time.Now(), sampleBody)); rec.Code != 200 {
				t.Errorf("%d", rec.Code)
			}
		}()
	}
	wg.Wait()
}
