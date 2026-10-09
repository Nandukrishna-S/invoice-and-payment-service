package payments

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const paymentColumns = `payment_ref_id, amount_cents, status, psp_ref_id, failure_code, created_at, resolved_at`

func (repo) insert(ctx context.Context, q db.Querier, p Payment) (Payment, error) {
	return scan(q.QueryRow(ctx,
		`INSERT INTO payments (payment_ref_id, amount_cents) VALUES ($1, $2) RETURNING `+paymentColumns,
		p.RefID, p.AmountCents))
}

func (repo) get(ctx context.Context, q db.Querier, ref uuid.UUID) (Payment, error) {
	p, err := scan(q.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE payment_ref_id = $1`, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	return p, err
}

// resolve records a definitive outcome, but only on a payment that is still
// pending. It reports whether this call was the one that resolved it, so
// racing resolvers (the inline call and the reconciler) apply at most once.
func (repo) resolve(ctx context.Context, q db.Querier, ref uuid.UUID, status Status, pspRef, failureCode *string) (bool, error) {
	tag, err := q.Exec(ctx,
		`UPDATE payments
		 SET status = $2, psp_ref_id = $3, failure_code = $4, resolved_at = now()
		 WHERE payment_ref_id = $1 AND status = 'pending'`,
		ref, status, pspRef, failureCode)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func scan(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.RefID, &p.AmountCents, &p.Status, &p.PSPRefID, &p.FailureCode, &p.CreatedAt, &p.ResolvedAt)
	return p, err
}

// pendingPage returns pending payments at least minAge old, oldest first, after
// the cursor. Keyset paging walks all of them each sweep, so payments that stay
// pending (say, ones the provider has never heard of) cannot starve newer ones.
func (repo) pendingPage(ctx context.Context, q db.Querier, minAge time.Duration, after PendingCursor, limit int) ([]PendingRef, error) {
	rows, err := q.Query(ctx,
		`SELECT payment_ref_id, created_at FROM payments
		 WHERE status = 'pending'
		   AND created_at <= now() - ($1::bigint * interval '1 millisecond')
		   AND (created_at, payment_ref_id) > ($2, $3)
		 ORDER BY created_at, payment_ref_id
		 LIMIT $4`,
		minAge.Milliseconds(), after.CreatedAt, after.RefID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingRef
	for rows.Next() {
		var p PendingRef
		if err := rows.Scan(&p.RefID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
