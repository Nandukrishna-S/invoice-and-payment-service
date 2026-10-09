// Package mockpsp is a stand-in payment service provider for the demo. It is not
// part of the product: it exists so the invoice service has something to call
// whose failures (declines, timeouts, dropped connections) can be provoked on
// demand with magic card tokens.
//
// Contract (all JSON, no authentication; a simplification, since a real PSP
// authenticates every call):
//
//	POST /charges            Idempotency-Key header, body {token, amount_cents}
//	GET  /charges/{key}      look a charge up by the idempotency key it was made with
//
// A charge is identified by its idempotency key. Repeating a key with the same
// parameters returns the original charge (never a second one); with different
// parameters it is a 409. A charge is `processing`, `succeeded` or `failed`.
package mockpsp

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusProcessing = "processing"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"

	FailureInsufficientFunds = "insufficient_funds"
	FailureCardDeclined      = "card_declined"
)

type Charge struct {
	IdempotencyKey string
	PSPRefID       string
	Token          string
	AmountCents    int64
	Status         string
	FailureCode    *string
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// behaviour is what a card token makes the PSP do.
type behaviour struct {
	status      string // the final outcome
	failureCode string
	// slow charges are stored as processing and only settle after the processing
	// delay, long after a caller with a 5s timeout has given up.
	slow bool
	// dropConnection processes the charge, then hangs up without answering, so
	// the caller cannot tell whether it was charged.
	dropConnection bool
}

func behaviourFor(token string) behaviour {
	switch token {
	case "tok_success":
		return behaviour{status: StatusSucceeded}
	case "tok_insufficient_funds":
		return behaviour{status: StatusFailed, failureCode: FailureInsufficientFunds}
	case "tok_timeout":
		return behaviour{status: StatusSucceeded, slow: true}
	case "tok_network_error":
		return behaviour{status: StatusSucceeded, dropConnection: true}
	default: // tok_card_declined, and any token we don't recognise
		return behaviour{status: StatusFailed, failureCode: FailureCardDeclined}
	}
}

func newPSPRefID() string { return "ch_" + uuid.Must(uuid.NewV7()).String() }
