// Package testpsp starts the real mock payment provider, with its own schema and
// database, for tests that exercise the invoice service end to end over HTTP.
package testpsp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/mockpsp"
	"invoice-and-payment-service/internal/testutil"
	"invoice-and-payment-service/migrations/psp"
)

// Fault is a way the provider can misbehave. The fault is injected in front of
// the real mock, so the request never reaches it: nothing is charged.
type Fault int32

const (
	FaultNone        Fault = iota
	FaultUnavailable       // answers 503
	FaultDropped           // hangs up without answering
	FaultHang              // never answers
)

type Mock struct {
	URL  string
	Pool *pgxpool.Pool

	fault atomic.Int32
}

// SetFault switches the provider's misbehaviour on or off.
func (m *Mock) SetFault(f Fault) { m.fault.Store(int32(f)) }

// Start serves the mock. processingDelay is how long tok_timeout charges stay
// processing; success and decline answers are immediate.
func Start(t testing.TB, processingDelay time.Duration) *Mock {
	t.Helper()
	url := testutil.NewSchema(t)
	if err := db.Migrate(url, psp.FS, "schema_migrations"); err != nil {
		t.Fatalf("migrate mock psp: %v", err)
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
	m := &Mock{Pool: pool}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch Fault(m.fault.Load()) {
		case FaultUnavailable:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case FaultDropped:
			panic(http.ErrAbortHandler)
		case FaultHang:
			_, _ = io.Copy(io.Discard, req.Body) // the server only notices a disconnect once the body is read
			<-req.Context().Done()
			return
		}
		r.ServeHTTP(w, req)
	}))
	m.URL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		cancel()
		svc.Close()
		pool.Close()
	})
	return m
}

// Charges counts the charges the provider has recorded: one per idempotency key.
func (m *Mock) Charges(t testing.TB) int {
	t.Helper()
	var n int
	if err := m.Pool.QueryRow(context.Background(), `SELECT count(*) FROM charges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
