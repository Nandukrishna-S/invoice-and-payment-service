// Package testpsp starts the real mock payment provider, with its own schema and
// database, for tests that exercise the invoice service end to end over HTTP.
package testpsp

import (
	"context"
	"net/http/httptest"
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

type Mock struct {
	URL  string
	Pool *pgxpool.Pool
}

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
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		svc.Close()
		pool.Close()
	})
	return &Mock{URL: srv.URL, Pool: pool}
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
