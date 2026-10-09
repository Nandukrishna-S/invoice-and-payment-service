package invoices

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/apperr"
)

const (
	maxLineItems      = 100
	maxDescriptionLen = 500
)

type Service struct {
	pool *pgxpool.Pool
	repo repo
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

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
		return s.repo.insertTransition(ctx, tx, inv.ID, nil, StatusDraft, "created")
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
