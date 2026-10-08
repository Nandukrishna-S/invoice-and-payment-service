package db

import (
	"context"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"

	"invoice-and-payment-service/internal/testutil"
	"invoice-and-payment-service/migrations"
)

var fakeMigrations = fstest.MapFS{
	"000001_widgets.up.sql":   {Data: []byte("CREATE TABLE widgets (id BIGINT PRIMARY KEY);")},
	"000001_widgets.down.sql": {Data: []byte("DROP TABLE widgets;")},
}

func TestMigrateRoundTrip(t *testing.T) {
	url := testutil.NewSchema(t)
	ctx := context.Background()

	pool, err := NewPool(ctx, url, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	exists := func() bool {
		var ok bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = current_schema() AND table_name = 'widgets')`).Scan(&ok)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if err := Migrate(url, fakeMigrations, "schema_migrations"); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !exists() {
		t.Fatal("table missing after up")
	}
	if err := Migrate(url, fakeMigrations, "schema_migrations"); err != nil {
		t.Fatalf("second up should be a no-op: %v", err)
	}

	m, err := newMigrate(url, fakeMigrations, "schema_migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	if exists() {
		t.Fatal("table still present after down")
	}

	if err := Migrate(url, fakeMigrations, "schema_migrations"); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !exists() {
		t.Fatal("table missing after second up")
	}
}

func TestMigrateEmptySetIsNotAnError(t *testing.T) {
	url := testutil.NewSchema(t)
	if err := Migrate(url, fstest.MapFS{}, "schema_migrations"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateFailureIsReported(t *testing.T) {
	url := testutil.NewSchema(t)
	bad := fstest.MapFS{
		"000001_bad.up.sql":   {Data: []byte("THIS IS NOT SQL;")},
		"000001_bad.down.sql": {Data: []byte("SELECT 1;")},
	}
	if err := Migrate(url, bad, "schema_migrations"); err == nil {
		t.Fatal("expected an error from invalid SQL")
	}
}

// The embedded production migrations must always survive up -> down -> up.
func TestEmbeddedMigrationsRoundTrip(t *testing.T) {
	url := testutil.NewSchema(t)
	var fsys fs.FS = migrations.FS

	if err := Migrate(url, fsys, "schema_migrations"); err != nil {
		t.Fatalf("up: %v", err)
	}
	m, err := newMigrate(url, fsys, "schema_migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && err.Error() != "no change" {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(url, fsys, "schema_migrations"); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// search_path must be set on every pooled connection, not just the first one.
func TestPoolAppliesSearchPathToEveryConnection(t *testing.T) {
	url := testutil.NewSchema(t)
	ctx := context.Background()

	pool, err := NewPool(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const n = 4
	var wg, acquired sync.WaitGroup
	paths := make(chan string, n)
	acquired.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			acquired.Done()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Release()
			acquired.Wait() // all n are held at once, so the pool opened n distinct connections
			var sp string
			if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&sp); err != nil {
				t.Error(err)
				return
			}
			paths <- sp
		}()
	}
	wg.Wait()
	close(paths)

	first := ""
	count := 0
	for p := range paths {
		count++
		if first == "" {
			first = p
		}
		if p != first || p == "" || p == `"$user", public` {
			t.Fatalf("connection has search_path %q, want the test schema on all connections (first=%q)", p, first)
		}
	}
	if count != n {
		t.Fatalf("checked %d connections, want %d", count, n)
	}
}
