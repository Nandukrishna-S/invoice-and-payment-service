// Package invoices owns invoices, their line items and their status history.
package invoices

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusDraft         Status = "draft"
	StatusOpen          Status = "open"
	StatusPaid          Status = "paid"
	StatusVoid          Status = "void"
	StatusUncollectible Status = "uncollectible"
)

var allStatuses = []Status{StatusDraft, StatusOpen, StatusPaid, StatusVoid, StatusUncollectible}

func (s Status) valid() bool {
	for _, v := range allStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// LineInput is what a client supplies for one line.
type LineInput struct {
	Description     string
	Quantity        int64
	UnitAmountCents int64
}

// LineItem is a line with the server-computed amount.
type LineItem struct {
	Description     string
	Quantity        int64
	UnitAmountCents int64
	AmountCents     int64
}

type Invoice struct {
	ID             uuid.UUID
	BusinessID     uuid.UUID
	CustomerID     uuid.UUID
	Status         Status
	Currency       string
	TotalCents     int64
	DueDate        time.Time // a calendar date, held as UTC midnight
	SequenceNumber *string
	PaidAt         *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LineItems      []LineItem
}

// CreateInput is a create request after parsing, before validation.
type CreateInput struct {
	CustomerID         uuid.UUID
	DueDate            time.Time
	Lines              []LineInput
	ExpectedTotalCents int64
}
