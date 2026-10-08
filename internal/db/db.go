// Package db owns the connection pool and the migration runner.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5 driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx, so repos run the same SQL
// inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewPool opens the single application pool and verifies connectivity.
// Connection-level settings in the URL (such as search_path) are applied to
// every pooled connection as runtime parameters.
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending up migrations from the root of migrations.
// table names the golang-migrate bookkeeping table, so several services can
// share one database. An empty migration set is not an error.
func Migrate(databaseURL string, migrations fs.FS, table string) error {
	m, err := newMigrate(databaseURL, migrations, table)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	err = m.Up()
	if errors.Is(err, migrate.ErrNoChange) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func newMigrate(databaseURL string, migrations fs.FS, table string) (*migrate.Migrate, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", table)
	u.RawQuery = q.Encode()

	src, err := iofs.New(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("open migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, fmt.Errorf("open migrator: %w", err)
	}
	return m, nil
}
