package psp

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/mockpsp"
	"invoice-and-payment-service/internal/testutil"
	"invoice-and-payment-service/migrations/psp"
)

// These tests run the client against the real mock provider and its real
// database, so the wire format (psp_ref, code) is checked end to end.

// startMock serves the mock with the given delay for tok_timeout charges.
func startMock(t *testing.T, processingDelay time.Duration) string {
	t.Helper()
	url := testutil.NewSchema(t)
	if err := db.Migrate(url, psp.FS, "schema_migrations"); err != nil {
		t.Fatal(err)
	}
	pool, err := db.NewPool(context.Background(), url, 20)
	if err != nil {
		t.Fatal(err)
	}
	bg, cancel := context.WithCancel(context.Background())
	svc := mockpsp.NewService(pool, bg, processingDelay, 0)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	mockpsp.RegisterRoutes(r, svc)
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		svc.Close()
		pool.Close()
	})
	return srv.URL
}

func newRef() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func TestRealProviderTokens(t *testing.T) {
	c := NewClient(startMock(t, time.Hour), time.Second, 2*time.Second)
	tests := []struct {
		token string
		want  Status
		code  string
	}{
		{"tok_success", Succeeded, ""},
		{"tok_insufficient_funds", Failed, CodeInsufficientFunds},
		{"tok_card_declined", Failed, CodeCardDeclined},
		{"tok_never_heard_of_it", Failed, CodeCardDeclined},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			ref := newRef()
			got := c.Charge(context.Background(), ref, tt.token, 4500)
			if got.Status != tt.want || got.Code != tt.code {
				t.Fatalf("got %+v, want %s/%q", got, tt.want, tt.code)
			}
			if tt.want == Succeeded {
				if _, err := uuid.Parse(got.PSPRef); err != nil {
					t.Fatalf("psp_ref %q should be the provider's UUID", got.PSPRef)
				}
			}
			// Asking again gives the same answer: the provider is the source of truth.
			if q := c.Query(context.Background(), ref); q.Status != got.Status || q.PSPRef != got.PSPRef || q.Code != got.Code {
				t.Fatalf("Query: %+v, want %+v", q, got)
			}
		})
	}
}

func TestRealProviderRepeatedChargeNeverChargesTwice(t *testing.T) {
	c := NewClient(startMock(t, time.Hour), time.Second, 2*time.Second)
	ref := newRef()
	first := c.Charge(context.Background(), ref, "tok_success", 4500)
	again := c.Charge(context.Background(), ref, "tok_success", 4500)
	if first.Status != Succeeded || again.PSPRef != first.PSPRef {
		t.Fatalf("first %+v, again %+v: same ref expected", first, again)
	}
	// The same key with other parameters is our bug; the provider says 409 and we must not guess.
	if got := c.Charge(context.Background(), ref, "tok_success", 1); got.Status != Unknown {
		t.Fatalf("a rejected request must be Unknown, got %+v", got)
	}
}

func TestRealProviderUnknownChargeIsNotFound(t *testing.T) {
	c := NewClient(startMock(t, time.Hour), time.Second, 2*time.Second)
	if got := c.Query(context.Background(), newRef()); got.Status != NotFound {
		t.Fatalf("got %+v", got)
	}
}

// tok_timeout: the provider takes far longer than we wait. We must come back
// at our deadline with "unknown", not "failed", and later find it succeeded.
func TestRealProviderTimeoutIsUnknownThenResolvesToSucceeded(t *testing.T) {
	c := NewClient(startMock(t, 700*time.Millisecond), time.Second, 250*time.Millisecond)
	ref := newRef()

	start := time.Now()
	got := c.Charge(context.Background(), ref, "tok_timeout", 4500)
	if got.Status != Unknown || got.Definitive() {
		t.Fatalf("got %+v, want Unknown", got)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > time.Second {
		t.Fatalf("returned after %v, want about the 250ms budget", elapsed)
	}

	if q := c.Query(context.Background(), ref); q.Status != Processing {
		t.Fatalf("while the provider is working: %+v, want Processing", q)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		q := c.Query(context.Background(), ref)
		if q.Status == Succeeded && q.PSPRef != "" {
			return
		}
		if q.Status == Failed || time.Now().After(deadline) {
			t.Fatalf("expected the charge to settle as succeeded, got %+v", q)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// tok_network_error: the provider charges, then hangs up. We can't know that
// from the response, only by asking afterwards.
func TestRealProviderDroppedConnectionIsUnknownButTheChargeHappened(t *testing.T) {
	// A fresh client per case: Go retries a dropped request transparently on a reused
	// connection, which would hide the drop. Here the first request opens the connection.
	base := startMock(t, time.Hour)
	ref := newRef()

	got := NewClient(base, time.Second, 2*time.Second).Charge(context.Background(), ref, "tok_network_error", 4500)
	if got.Status != Unknown {
		t.Fatalf("got %+v, want Unknown", got)
	}
	q := NewClient(base, time.Second, 2*time.Second).Query(context.Background(), ref)
	if q.Status != Succeeded || q.PSPRef == "" {
		t.Fatalf("the charge happened, Query must say so: %+v", q)
	}
}
