// Package db owns the connection pool and the migration runner.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"time"

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
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = connectTimeout
	}
	// Bounds the first connection too, so a database that accepts and then
	// stalls can't hang startup.
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
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

const connectTimeout = 10 * time.Second

// Migrate applies all pending up migrations from the root of migrations.
// table names the golang-migrate bookkeeping table, so several services can
// share one database. An empty migration set on a fresh database is not an
// error; a database already ahead of the binary's migrations is.
func Migrate(databaseURL string, migrations fs.FS, table string) error {
	m, err := newMigrate(databaseURL, migrations, table)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	_, _, versionErr := m.Version()
	fresh := errors.Is(versionErr, migrate.ErrNilVersion)

	err = m.Up()
	switch {
	case err == nil, errors.Is(err, migrate.ErrNoChange):
		return nil
	case fresh && errors.Is(err, os.ErrNotExist):
		// No migration files at all. On a database that already has a version, the
		// same error means the binary is older than the schema, which must fail.
		return nil
	}
	return fmt.Errorf("apply migrations: %w", err)
}

func newMigrate(databaseURL string, migrations fs.FS, table string) (*migrate.Migrate, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		// url.Error quotes the whole URL, password included; keep only the reason.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", table)
	if q.Get("connect_timeout") == "" {
		q.Set("connect_timeout", strconv.Itoa(int(connectTimeout.Seconds())))
	}
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
