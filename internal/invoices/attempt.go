package invoices

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
)

type AttemptStatus string

const (
	AttemptPending   AttemptStatus = "pending"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
)

// Attempt is one try at paying an invoice: the invoice-side status plus the
// amount and provider reference from its payment record.
type Attempt struct {
	ID           uuid.UUID
	InvoiceID    uuid.UUID
	PaymentRefID uuid.UUID
	AmountCents  int64
	Status       AttemptStatus
	FailureCode  *string
	PSPRefID     *string
	CreatedAt    time.Time
	ResolvedAt   *time.Time
}

// Resolved reports whether the provider has given a final answer.
func (a Attempt) Resolved() bool { return a.Status != AttemptPending }

// PayInput is a pay request after parsing.
type PayInput struct {
	InvoiceID      uuid.UUID
	IdempotencyKey string
	CardToken      string
	AmountCents    int64
}

// payRequestHash identifies a pay request for idempotency: the same invoice,
// card token and amount. It hashes the parsed values, so whitespace and field
// order in the JSON don't matter. The token is hashed together with the invoice
// id and is never stored.
func payRequestHash(invoiceID uuid.UUID, cardToken string, amountCents int64) []byte {
	h := sha256.New()
	h.Write([]byte("pay/v1"))
	h.Write(invoiceID[:])
	// Length-prefix the token so no two different requests share an encoding.
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(cardToken)))
	h.Write(n[:])
	h.Write([]byte(cardToken))
	binary.BigEndian.PutUint64(n[:], uint64(amountCents))
	h.Write(n[:])
	return h.Sum(nil)
}
