package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/psp"
)

// Service works on whatever Querier it is handed, so callers decide the
// transaction: the pay flow creates a payment inside its reservation
// transaction, and resolves it in a separate one.
type Service struct {
	repo repo
}

func NewService() *Service { return &Service{} }

// Create records a new pending payment of amountCents.
func (s *Service) Create(ctx context.Context, q db.Querier, amountCents int64) (Payment, error) {
	if amountCents < 1 {
		return Payment{}, fmt.Errorf("payment amount must be positive, got %d", amountCents)
	}
	return s.repo.insert(ctx, q, Payment{RefID: uuid.Must(uuid.NewV7()), AmountCents: amountCents})
}

func (s *Service) Get(ctx context.Context, q db.Querier, ref uuid.UUID) (Payment, error) {
	return s.repo.get(ctx, q, ref)
}

// Resolve moves a pending payment to succeeded or failed from a definitive
// provider outcome. It returns false, changing nothing, if the payment was
// already resolved. Anything the provider did not definitively answer
// (unknown, processing, not found) is refused with ErrNotDefinitive, so a
// timeout can never be recorded as a failure.
func (s *Service) Resolve(ctx context.Context, q db.Querier, ref uuid.UUID, o psp.Outcome) (bool, error) {
	switch {
	case o.Status == psp.Succeeded && o.PSPRef != "":
		return s.repo.resolve(ctx, q, ref, StatusSucceeded, &o.PSPRef, nil)
	case o.Status == psp.Failed && o.Code != "":
		return s.repo.resolve(ctx, q, ref, StatusFailed, nil, &o.Code)
	default:
		return false, fmt.Errorf("%w: %q", ErrNotDefinitive, o.Status)
	}
}
