// Package webhooks registers the URLs a business wants notified, and owns the
// transactional outbox that queues one delivery per event per active endpoint.
// Delivering the queued rows is the delivery worker's job (a later checkpoint).
package webhooks

import (
	"time"

	"github.com/google/uuid"
)

// Event types a business can receive.
const (
	EventInvoiceCreated       = "invoice.created"
	EventInvoicePaid          = "invoice.paid"
	EventInvoicePaymentFailed = "invoice.payment_failed"
)

type Endpoint struct {
	ID         uuid.UUID
	BusinessID uuid.UUID
	URL        string
	// Secret is the signing secret. It is returned to the client once, at
	// registration, and never in any later response.
	Secret     string
	CreatedAt  time.Time
	DisabledAt *time.Time
}
