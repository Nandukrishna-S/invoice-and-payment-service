package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const seedTimeout = 15 * time.Second

type Service struct {
	pool *pgxpool.Pool
	repo repo
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Authenticate resolves a raw key to its principal. Every failure that is the
// caller's fault returns ErrUnauthorized; only database errors differ.
func (s *Service) Authenticate(ctx context.Context, rawKey string) (Principal, error) {
	if !ValidKeyFormat(rawKey) {
		return Principal{}, ErrUnauthorized
	}
	k, err := s.repo.findKeyByHash(ctx, s.pool, HashKey(rawKey))
	if errors.Is(err, errKeyNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("look up api key: %w", err)
	}
	if k.RevokedAt != nil {
		// A revoked key still matches by hash; its use is a leak signal.
		slog.WarnContext(ctx, "revoked api key used",
			"api_key_id", k.ID, "business_id", k.BusinessID, "api_key_prefix", k.Prefix)
		return Principal{}, ErrUnauthorized
	}
	return Principal{BusinessID: k.BusinessID, APIKeyID: k.ID}, nil
}

// Seed creates the first business and its API key when no business exists.
// With demoKey set (local development only) that value becomes the key, so
// documented curl examples work as is. Otherwise a random key is generated and
// returned as generatedKey, which the caller must show once. Seed does nothing,
// and generatedKey is empty, when a business already exists.
func (s *Service) Seed(ctx context.Context, demoKey string) (generatedKey string, err error) {
	key := demoKey
	if key != "" && !ValidKeyFormat(key) {
		return "", fmt.Errorf("DEMO_API_KEY must be %q followed by 64 lowercase hex characters", keyPrefix)
	}

	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Without the lock, replicas starting together could each see an empty table.
	if err := s.repo.lockBusinesses(ctx, tx); err != nil {
		return "", err
	}
	exists, err := s.repo.anyBusiness(ctx, tx)
	if err != nil || exists {
		return "", err
	}

	if key == "" {
		if key, err = GenerateKey(); err != nil {
			return "", err
		}
		generatedKey = key
	}
	business := uuid.Must(uuid.NewV7())
	apiKey := APIKey{
		ID:         uuid.Must(uuid.NewV7()),
		BusinessID: business,
		Prefix:     displayPrefix(key),
		KeyHash:    HashKey(key),
	}
	if err := s.repo.insertBusiness(ctx, tx, business, "Demo Business"); err != nil {
		return "", err
	}
	if err := s.repo.insertKey(ctx, tx, apiKey); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "seeded first business",
		"business_id", business, "api_key_id", apiKey.ID, "api_key_prefix", apiKey.Prefix)
	return generatedKey, nil
}
