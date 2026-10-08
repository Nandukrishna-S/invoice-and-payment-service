// Package testdb gives integration tests a migrated pool on a private schema.
// It is separate from testutil because db's own tests import testutil.
package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/testutil"
	"invoice-and-payment-service/migrations"
)

// New returns a pool whose schema has all API migrations applied. The test is
// skipped when TEST_DATABASE_URL is unset.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := testutil.NewSchema(t)
	if err := db.Migrate(url, migrations.FS, "schema_migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.NewPool(context.Background(), url, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
