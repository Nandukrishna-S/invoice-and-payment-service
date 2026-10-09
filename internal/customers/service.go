package customers

import (
	"context"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/db"
)

const (
	maxNameLen  = 200
	maxEmailLen = 320
)

type Service struct {
	db   db.Querier
	repo repo
}

func NewService(q db.Querier) *Service { return &Service{db: q} }

func (s *Service) Create(ctx context.Context, businessID uuid.UUID, name, email string) (Customer, error) {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	if err := validate(name, email); err != nil {
		return Customer{}, err
	}
	return s.repo.insert(ctx, s.db, Customer{
		ID:         uuid.Must(uuid.NewV7()),
		BusinessID: businessID,
		Name:       name,
		Email:      email,
	})
}

func (s *Service) Get(ctx context.Context, businessID, id uuid.UUID) (Customer, error) {
	return s.repo.get(ctx, s.db, businessID, id)
}

// List returns one page, newest first, and whether more exist after it.
func (s *Service) List(ctx context.Context, businessID uuid.UUID, after *uuid.UUID, limit int) ([]Customer, bool, error) {
	// One extra row tells us whether another page exists without a count query.
	cs, err := s.repo.list(ctx, s.db, businessID, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(cs) > limit {
		return cs[:limit], true, nil
	}
	return cs, false, nil
}

func validate(name, email string) error {
	switch {
	case name == "":
		return apperr.Validation("name", "is required")
	case hasControlChars(name):
		// Postgres TEXT rejects NUL, which would otherwise surface as a 500.
		return apperr.Validation("name", "must not contain control characters")
	case utf8.RuneCountInString(name) > maxNameLen:
		return apperr.Validation("name", "must be at most 200 characters")
	case email == "":
		return apperr.Validation("email", "is required")
	case hasControlChars(email):
		return apperr.Validation("email", "must not contain control characters")
	case utf8.RuneCountInString(email) > maxEmailLen:
		return apperr.Validation("email", "must be at most 320 characters")
	}
	// ParseAddress also accepts "Name <a@b.c>"; requiring the parsed address to
	// equal the input rejects that form.
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return apperr.Validation("email", "must be a valid email address")
	}
	return nil
}

func hasControlChars(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}
