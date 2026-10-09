package invoices

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/payments"
)

const (
	maxLineItems      = 100
	maxDescriptionLen = 500
)

type Service struct {
	pool *pgxpool.Pool
	repo repo
	// fiscalLoc decides which financial year a finalize belongs to; now is
	// replaceable so tests can cross year boundaries.
	fiscalLoc *time.Location
	now       func() time.Time

	payments *payments.Service
	psp      PSPClient
}

func NewService(pool *pgxpool.Pool, fiscalLoc *time.Location, psp PSPClient) *Service {
	return &Service{pool: pool, fiscalLoc: fiscalLoc, now: time.Now, payments: payments.NewService(), psp: psp}
}

func (s *Service) Finalize(ctx context.Context, businessID, id uuid.UUID) (Invoice, error) {
	return s.apply(ctx, businessID, id, actionFinalize)
}

func (s *Service) Void(ctx context.Context, businessID, id uuid.UUID) (Invoice, error) {
	return s.apply(ctx, businessID, id, actionVoid)
}

func (s *Service) MarkUncollectible(ctx context.Context, businessID, id uuid.UUID) (Invoice, error) {
	return s.apply(ctx, businessID, id, actionMarkUncollected)
}

// apply performs one user-triggered status change: lock the invoice, check the
// state machine, update guarded by the old status, and append the transition,
// all in one transaction so a failure at any step leaves nothing behind.
func (s *Service) apply(ctx context.Context, businessID, id uuid.UUID, a action) (Invoice, error) {
	var out Invoice
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inv, err := s.repo.lockForUpdate(ctx, tx, businessID, id)
		if err != nil {
			return err
		}
		if !canTransition(inv.Status, a.target) {
			return invalidTransition(a, inv.Status)
		}
		// Taking an invoice out of collection while the provider may be charging the
		// customer would leave a payment with nowhere to land.
		if a.target == StatusVoid || a.target == StatusUncollectible {
			pending, err := s.repo.hasPendingAttempt(ctx, tx, inv.ID)
			if err != nil {
				return err
			}
			if pending {
				return ErrPaymentInProgress
			}
		}

		// Drafts hold no number; one is drawn only when the invoice is issued.
		var number *string
		if a.target == StatusOpen {
			n, err := s.nextInvoiceNumber(ctx, tx, businessID)
			if err != nil {
				return err
			}
			number = &n
		}

		updatedAt, err := s.repo.updateStatus(ctx, tx, inv.ID, inv.Status, a.target, number)
		if err != nil {
			return err
		}
		if err := s.repo.insertTransition(ctx, tx, inv.ID, &inv.Status, a.target, a.reason, nil); err != nil {
			return err
		}
		items, err := s.repo.lineItems(ctx, tx, businessID, []uuid.UUID{inv.ID})
		if err != nil {
			return err
		}

		inv.Status, inv.UpdatedAt, inv.LineItems = a.target, updatedAt, items[inv.ID]
		if number != nil {
			inv.SequenceNumber = number
		}
		out = inv
		return nil
	})
	return out, err
}

func (s *Service) nextInvoiceNumber(ctx context.Context, tx pgx.Tx, businessID uuid.UUID) (string, error) {
	fy := fiscalYear(s.now(), s.fiscalLoc)
	prefix, n, err := s.repo.nextSequenceNumber(ctx, tx, businessID, fy)
	if err != nil {
		return "", err
	}
	return formatInvoiceNumber(prefix, n, fy), nil
}

// Create validates the request, computes the totals on the server, checks them
// against the client's expectation, and stores the draft, its lines and the
// creation transition in one transaction.
func (s *Service) Create(ctx context.Context, businessID uuid.UUID, in CreateInput) (Invoice, error) {
	lines, err := validateCreate(in)
	if err != nil {
		return Invoice{}, err
	}
	items, total, err := ComputeTotals(lines)
	if err != nil {
		return Invoice{}, err
	}
	// The client value is an assertion only; it is compared here and never stored.
	if in.ExpectedTotalCents != total {
		return Invoice{}, ErrTotalMismatch
	}

	inv := Invoice{
		ID:         uuid.Must(uuid.NewV7()),
		BusinessID: businessID,
		CustomerID: in.CustomerID,
		Status:     StatusDraft,
		Currency:   "USD",
		TotalCents: total,
		DueDate:    in.DueDate,
		LineItems:  items,
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.repo.insert(ctx, tx, &inv); err != nil {
			return err
		}
		if err := s.repo.insertLineItems(ctx, tx, inv.ID, inv.LineItems); err != nil {
			return err
		}
		return s.repo.insertTransition(ctx, tx, inv.ID, nil, StatusDraft, "created", nil)
	})
	if err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

func (s *Service) Get(ctx context.Context, businessID, id uuid.UUID) (Invoice, error) {
	return s.repo.get(ctx, s.pool, businessID, id)
}

// List returns one page, newest first, and whether more exist after it.
func (s *Service) List(ctx context.Context, businessID uuid.UUID, status *Status, after *uuid.UUID, limit int) ([]Invoice, bool, error) {
	// One extra row tells us whether another page exists without a count query.
	invs, err := s.repo.list(ctx, s.pool, businessID, status, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(invs) > limit {
		return invs[:limit], true, nil
	}
	return invs, false, nil
}

// validateCreate checks field-level rules and returns the lines with trimmed descriptions.
func validateCreate(in CreateInput) ([]LineInput, error) {
	if in.ExpectedTotalCents < 1 {
		return nil, apperr.Validation("expected_total_cents", "must be at least 1")
	}
	if n := len(in.Lines); n < 1 || n > maxLineItems {
		return nil, apperr.Validation("line_items", fmt.Sprintf("must contain between 1 and %d items", maxLineItems))
	}
	lines := make([]LineInput, len(in.Lines))
	for i, l := range in.Lines {
		field := func(name string) string { return fmt.Sprintf("line_items[%d].%s", i, name) }
		desc := strings.TrimSpace(l.Description)
		switch {
		case desc == "":
			return nil, apperr.Validation(field("description"), "is required")
		case utf8.RuneCountInString(desc) > maxDescriptionLen:
			return nil, apperr.Validation(field("description"), fmt.Sprintf("must be at most %d characters", maxDescriptionLen))
		case strings.IndexFunc(desc, unicode.IsControl) >= 0:
			// Postgres TEXT rejects NUL, which would otherwise surface as a 500.
			return nil, apperr.Validation(field("description"), "must not contain control characters")
		case l.Quantity < 1:
			return nil, apperr.Validation(field("quantity"), "must be at least 1")
		case l.UnitAmountCents < 1:
			return nil, apperr.Validation(field("unit_amount_cents"), "must be at least 1")
		}
		lines[i] = LineInput{Description: desc, Quantity: l.Quantity, UnitAmountCents: l.UnitAmountCents}
	}
	return lines, nil
}
