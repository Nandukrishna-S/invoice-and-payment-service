package customers

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const customerColumns = `id, business_id, name, email, created_at`

func scan(row pgx.Row) (Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.BusinessID, &c.Name, &c.Email, &c.CreatedAt)
	return c, err
}

func (repo) insert(ctx context.Context, q db.Querier, c Customer) (Customer, error) {
	return scan(q.QueryRow(ctx,
		`INSERT INTO customers (id, business_id, name, email) VALUES ($1, $2, $3, $4)
		 RETURNING `+customerColumns,
		c.ID, c.BusinessID, c.Name, c.Email))
}

func (repo) get(ctx context.Context, q db.Querier, businessID, id uuid.UUID) (Customer, error) {
	c, err := scan(q.QueryRow(ctx,
		`SELECT `+customerColumns+` FROM customers WHERE business_id = $1 AND id = $2`,
		businessID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrNotFound
	}
	return c, err
}

// list returns up to limit customers, newest first. UUIDv7 ids sort by
// creation time, so the id is the keyset cursor. after is only a position: it
// is never looked up, so it reveals nothing about other businesses' ids.
func (repo) list(ctx context.Context, q db.Querier, businessID uuid.UUID, after *uuid.UUID, limit int) ([]Customer, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = q.Query(ctx,
			`SELECT `+customerColumns+` FROM customers
			 WHERE business_id = $1 ORDER BY id DESC LIMIT $2`,
			businessID, limit)
	} else {
		rows, err = q.Query(ctx,
			`SELECT `+customerColumns+` FROM customers
			 WHERE business_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`,
			businessID, *after, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Customer
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
