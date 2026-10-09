package mockpsp

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const chargeColumns = `idempotency_key, psp_ref_id, token, amount_cents, status, failure_code, created_at, completed_at`

func scan(row pgx.Row) (Charge, error) {
	var c Charge
	err := row.Scan(&c.IdempotencyKey, &c.PSPRefID, &c.Token, &c.AmountCents, &c.Status,
		&c.FailureCode, &c.CreatedAt, &c.CompletedAt)
	return c, err
}

// insertIfAbsent stores a new charge. If the key already exists the stored charge
// is returned instead, with created=false; two racing callers cannot both insert.
func (r repo) insertIfAbsent(ctx context.Context, q db.Querier, c Charge) (stored Charge, created bool, err error) {
	stored, err = scan(q.QueryRow(ctx,
		`INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, failure_code, completed_at)
		 VALUES ($1, $2, $3, $4, $5::text, $6, CASE WHEN $5::text = 'processing' THEN NULL ELSE now() END)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+chargeColumns,
		c.IdempotencyKey, c.PSPRefID, c.Token, c.AmountCents, c.Status, c.FailureCode))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, err
	}
	stored, err = r.get(ctx, q, c.IdempotencyKey)
	return stored, false, err
}

func (repo) get(ctx context.Context, q db.Querier, key string) (Charge, error) {
	c, err := scan(q.QueryRow(ctx, `SELECT `+chargeColumns+` FROM charges WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, ErrNotFound
	}
	return c, err
}

// settle moves a processing charge to succeeded. Only a still-processing charge
// can change, so a charge that already settled is never touched. When minAge is
// positive the charge must also be at least that old.
func (repo) settle(ctx context.Context, q db.Querier, key string, minAge time.Duration) error {
	_, err := q.Exec(ctx,
		`UPDATE charges SET status = 'succeeded', completed_at = now()
		 WHERE idempotency_key = $1 AND status = 'processing'
		   AND created_at + ($2::bigint * interval '1 millisecond') <= now()`,
		key, minAge.Milliseconds())
	return err
}
