package invoices

import (
	"errors"
	"fmt"
	"net/http"

	"invoice-and-payment-service/internal/apperr"
)

var (
	ErrNotFound       = apperr.New(http.StatusNotFound, "invoice_not_found", "invoice not found")
	ErrTotalMismatch  = apperr.New(http.StatusUnprocessableEntity, "total_mismatch", "expected_total_cents does not match the total computed from line_items")
	ErrAmountOverflow = apperr.New(http.StatusUnprocessableEntity, "amount_overflow", "line item amount or invoice total exceeds the supported range")

	ErrAttemptNotFound   = apperr.New(http.StatusNotFound, "payment_attempt_not_found", "payment attempt not found")
	ErrPaymentInProgress = apperr.New(http.StatusConflict, "payment_in_progress", "a payment attempt is already pending for this invoice")
	ErrAmountMismatch    = apperr.New(http.StatusUnprocessableEntity, "amount_mismatch", "amount_cents does not match the invoice total")
	ErrKeyReused         = apperr.New(http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was already used with a different request")
	ErrKeyRequired       = apperr.New(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
)

// errNumberExhausted means a business used all 999999 numbers of a financial
// year. It is not the client's fault, so it surfaces as a 500 and is logged.
var errNumberExhausted = errors.New("invoice number counter exhausted for the financial year")

// errStaleStatus means the guarded UPDATE found the invoice in another state
// than the one just read under lock, which only direct database edits can cause.
var errStaleStatus = errors.New("invoice status changed while locked")

func invalidTransition(a action, from Status) *apperr.Error {
	return apperr.New(http.StatusConflict, "invalid_transition",
		fmt.Sprintf("cannot %s invoice in state %s", a.name, from))
}
