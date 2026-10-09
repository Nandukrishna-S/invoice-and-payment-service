// Package payments owns the money record: one row per attempt to collect a
// given amount, resolved at most once, with the provider as the only source of
// truth for the outcome.
//
// A payment knows nothing about invoices or businesses. Payment attempts link
// it to an invoice, so any read exposed to API clients must go through the
// attempt and the invoice's business_id; never expose a payment by its ref alone.
package payments

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Payment struct {
	// RefID is also the idempotency key used with the provider.
	RefID       uuid.UUID
	AmountCents int64
	Status      Status
	PSPRefID    *string
	FailureCode *string
	CreatedAt   time.Time
	ResolvedAt  *time.Time
}

// PendingRef identifies an in-flight payment for the reconciler.
type PendingRef struct {
	RefID     uuid.UUID
	CreatedAt time.Time
}

// PendingCursor is a position in the oldest-first list of pending payments.
// The zero value is the start.
type PendingCursor struct {
	CreatedAt time.Time
	RefID     uuid.UUID
}

func (p PendingRef) Cursor() PendingCursor {
	return PendingCursor{CreatedAt: p.CreatedAt, RefID: p.RefID}
}
