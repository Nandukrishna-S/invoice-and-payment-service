package psp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

var ref = uuid.MustParse("01a11fa0-0000-7000-8000-000000000001")

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, time.Second, 2*time.Second)
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestChargeMapsDefinitiveAnswers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Outcome
	}{
		{"success", `{"status":"succeeded","psp_ref":"abc-123"}`, Outcome{Status: Succeeded, PSPRef: "abc-123"}},
		{"insufficient funds", `{"status":"failed","code":"insufficient_funds"}`, Outcome{Status: Failed, Code: "insufficient_funds"}},
		{"declined", `{"status":"failed","code":"card_declined"}`, Outcome{Status: Failed, Code: "card_declined"}},
		{"processing is not an answer", `{"status":"processing"}`, Outcome{Status: Processing}},
		{"extra fields are ignored", `{"status":"succeeded","psp_ref":"r","amount_cents":1,"whatever":true}`, Outcome{Status: Succeeded, PSPRef: "r"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(t, reply(200, tt.body)).Charge(context.Background(), ref, "tok_x", 4500)
			if got.Status != tt.want.Status || got.PSPRef != tt.want.PSPRef || got.Code != tt.want.Code {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Anything the provider did not clearly answer must be Unknown, so callers keep
// the payment pending instead of guessing.
func TestChargeTreatsUncertaintyAsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"success without a reference", 200, `{"status":"succeeded"}`},
		{"failure with an unknown code", 200, `{"status":"failed","code":"fraud_suspected"}`},
		{"failure without a code", 200, `{"status":"failed"}`},
		{"unknown status", 200, `{"status":"refunded"}`},
		{"empty status", 200, `{}`},
		{"not json", 200, `<html>oops</html>`},
		{"truncated json", 200, `{"status":"succ`},
		{"empty body", 200, ``},
		{"500", 500, `{"status":"failed","code":"card_declined"}`},
		{"502", 502, ``},
		{"503", 503, `{"error":"overloaded"}`},
		{"400 means a bug on our side", 400, `{"error":{"code":"validation_failed"}}`},
		{"409 key reuse", 409, `{"error":{"code":"idempotency_conflict"}}`},
		{"422", 422, `{}`},
		{"404 on charge", 404, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serve(t, reply(tt.status, tt.body)).Charge(context.Background(), ref, "tok_x", 4500)
			if got.Status != Unknown || got.Definitive() || got.Cause == nil {
				t.Fatalf("got %+v, want Unknown with a cause", got)
			}
		})
	}
}

// Whatever the body says, a non-200 answer can never become Failed or Succeeded:
// this is the guard against marking a payment failed because of a proxy or crash.
func TestNoNon200StatusEverYieldsADefinitiveOutcome(t *testing.T) {
	bodies := []string{
		`{"status":"failed","code":"card_declined"}`,
		`{"status":"succeeded","psp_ref":"r"}`,
	}
	captureLogs(t) // the 4xx statuses log errors by design; keep them out of the test output
	for status := 100; status <= 599; status++ {
		if status == 200 || status == 201 || status == 204 || status == 205 || status == 304 || status/100 == 1 {
			continue // net/http constrains bodies on these, or they are the success path
		}
		for _, body := range bodies {
			c := serve(t, reply(status, body))
			if got := c.Charge(context.Background(), ref, "tok_x", 100); got.Definitive() {
				t.Fatalf("HTTP %d with body %s gave %+v", status, body, got)
			}
			if got := c.Query(context.Background(), ref); status != 404 && got.Definitive() {
				t.Fatalf("Query: HTTP %d with body %s gave %+v", status, body, got)
			}
		}
	}
}

func TestChargeSendsTheIdempotencyKeyAndOnlyWhatTheProviderNeeds(t *testing.T) {
	var gotKey, gotType, gotMethod, gotPath string
	var gotBody map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotType, gotMethod, gotPath = r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		reply(200, `{"status":"succeeded","psp_ref":"r"}`)(w, r)
	})
	c.Charge(context.Background(), ref, "tok_success", 4500)

	if gotMethod != "POST" || gotPath != "/charges" || gotType != "application/json" || gotKey != ref.String() {
		t.Fatalf("request: %s %s type=%q key=%q", gotMethod, gotPath, gotType, gotKey)
	}
	if len(gotBody) != 2 || gotBody["token"] != "tok_success" || gotBody["amount_cents"] != float64(4500) {
		t.Fatalf("body = %v, want exactly token and amount_cents", gotBody)
	}
}

func TestQuery(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   Status
	}{
		{"succeeded", 200, `{"status":"succeeded","psp_ref":"r"}`, Succeeded},
		{"failed", 200, `{"status":"failed","code":"card_declined"}`, Failed},
		{"still processing", 200, `{"status":"processing"}`, Processing},
		{"the provider has no such charge", 404, `{}`, NotFound},
		{"server error", 500, ``, Unknown},
		{"gateway error", 502, ``, Unknown},
		{"garbage", 200, `nope`, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				path = r.Method + " " + r.URL.Path
				reply(tt.status, tt.body)(w, r)
			})
			if got := c.Query(context.Background(), ref); got.Status != tt.want {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
			if path != "GET /charges/"+ref.String() {
				t.Fatalf("queried %q", path)
			}
		})
	}
}

func TestTransportFailuresAreUnknown(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(nil)
		url := srv.URL
		srv.Close()
		got := NewClient(url, time.Second, 2*time.Second).Charge(context.Background(), ref, "tok_x", 1)
		if got.Status != Unknown || got.Cause == nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("connection dropped without an answer", func(t *testing.T) {
		c := serve(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
		if got := c.Charge(context.Background(), ref, "tok_x", 1); got.Status != Unknown {
			t.Fatalf("got %+v", got)
		}
		if got := c.Query(context.Background(), ref); got.Status != Unknown {
			t.Fatalf("query: got %+v", got)
		}
	})
	t.Run("the caller cancels", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // the server only notices a disconnect once the body is read
			<-r.Context().Done()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if got := c.Charge(ctx, ref, "tok_x", 1); got.Status != Unknown || time.Since(start) > time.Second {
			t.Fatalf("got %+v after %v", got, time.Since(start))
		}
	})
	t.Run("the connection cannot be established in time", func(t *testing.T) {
		// A non-routable address: depending on the host this times out or is rejected
		// at once. Either way the call must end quickly and be Unknown.
		c := NewClient("http://10.255.255.1:81", 100*time.Millisecond, time.Second)
		start := time.Now()
		got := c.Charge(context.Background(), ref, "tok_x", 1)
		if got.Status != Unknown || time.Since(start) > 900*time.Millisecond {
			t.Fatalf("got %+v after %v", got, time.Since(start))
		}
	})
}

// The 5s budget must hold even when the provider sends nothing, or trickles bytes.
func TestTotalTimeoutBoundsEveryCall(t *testing.T) {
	var cancelled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server only notices a disconnect once the body is read
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, time.Second, 300*time.Millisecond)

	for name, call := range map[string]func() Outcome{
		"charge": func() Outcome { return c.Charge(context.Background(), ref, "tok_timeout", 1) },
		"query":  func() Outcome { return c.Query(context.Background(), ref) },
	} {
		start := time.Now()
		got := call()
		elapsed := time.Since(start)
		if got.Status != Unknown || elapsed < 250*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Errorf("%s: got %+v after %v, want Unknown after about 300ms", name, got, elapsed)
		}
	}
	if !cancelled.Load() {
		t.Error("the provider-side request should have been cancelled when we gave up")
	}
}

func TestSlowTrickledBodyIsAlsoBounded(t *testing.T) {
	c := NewClient(func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			for i := 0; i < 100; i++ {
				_, _ = io.WriteString(w, " ")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}(), time.Second, 300*time.Millisecond)

	start := time.Now()
	if got := c.Charge(context.Background(), ref, "tok_x", 1); got.Status != Unknown || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}
}

func TestOversizedResponseIsUnknown(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"succeeded","psp_ref":"`)
		_, _ = io.WriteString(w, strings.Repeat("a", 2<<20))
		_, _ = io.WriteString(w, `"}`)
	})
	if got := c.Charge(context.Background(), ref, "tok_x", 1); got.Status != Unknown {
		t.Fatalf("got %+v", got)
	}
}

// The card token goes to the provider and nowhere else.
func TestNeverLogsTheCardToken(t *testing.T) {
	logs := captureLogs(t)
	const token = "tok_super_secret_4242"
	for _, h := range []http.HandlerFunc{reply(400, `{}`), reply(200, `garbage`), reply(200, `{"status":"succeeded"}`), reply(200, `{"status":"failed","code":"x"}`), reply(500, ``)} {
		serve(t, h).Charge(context.Background(), ref, token, 1)
	}
	if out := logs.String(); !strings.Contains(out, "provider") || strings.Contains(out, token) {
		t.Fatalf("expected provider error logs without the token, got: %s", out)
	}
}

func TestUnexpected4xxIsLoggedAtErrorLevel(t *testing.T) {
	logs := captureLogs(t)
	serve(t, reply(409, `{}`)).Charge(context.Background(), ref, "tok_x", 1)
	if out := logs.String(); !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, ref.String()) {
		t.Fatalf("a rejected request is our bug and must be an error log naming the payment: %s", out)
	}

	logs.Reset()
	serve(t, reply(503, ``)).Charge(context.Background(), ref, "tok_x", 1)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("a 5xx is expected turbulence, not an error log: %s", logs.String())
	}
}

func TestNewClientWiresBothTimeouts(t *testing.T) {
	c := NewClient("http://x", 2*time.Second, 5*time.Second)
	if c.http.Timeout != 5*time.Second {
		t.Fatalf("total timeout = %v", c.http.Timeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil || tr.TLSHandshakeTimeout != 2*time.Second {
		t.Fatalf("connect timeouts are not wired: %+v", c.http.Transport)
	}
}
