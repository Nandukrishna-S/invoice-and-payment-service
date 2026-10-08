package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"invoice-and-payment-service/internal/db"
)

type repo struct{}

func (repo) findKeyByHash(ctx context.Context, q db.Querier, hash []byte) (APIKey, error) {
	k := APIKey{KeyHash: hash}
	err := q.QueryRow(ctx,
		`SELECT id, business_id, prefix, revoked_at FROM api_keys WHERE key_hash = $1`, hash,
	).Scan(&k.ID, &k.BusinessID, &k.Prefix, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, errKeyNotFound
	}
	return k, err
}

// lockBusinesses serialises concurrent seeders; it conflicts with itself but
// not with plain reads.
func (repo) lockBusinesses(ctx context.Context, q db.Querier) error {
	_, err := q.Exec(ctx, `LOCK TABLE businesses IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

func (repo) anyBusiness(ctx context.Context, q db.Querier) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM businesses)`).Scan(&exists)
	return exists, err
}

func (repo) insertBusiness(ctx context.Context, q db.Querier, id uuid.UUID, name string) error {
	_, err := q.Exec(ctx, `INSERT INTO businesses (id, name) VALUES ($1, $2)`, id, name)
	return err
}

func (repo) insertKey(ctx context.Context, q db.Querier, k APIKey) error {
	_, err := q.Exec(ctx,
		`INSERT INTO api_keys (id, business_id, prefix, key_hash) VALUES ($1, $2, $3, $4)`,
		k.ID, k.BusinessID, k.Prefix, k.KeyHash)
	return err
}
