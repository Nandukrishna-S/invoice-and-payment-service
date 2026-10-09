package invoices

import (
	"net/http"

	"invoice-and-payment-service/internal/apperr"
)

var (
	ErrNotFound       = apperr.New(http.StatusNotFound, "invoice_not_found", "invoice not found")
	ErrTotalMismatch  = apperr.New(http.StatusUnprocessableEntity, "total_mismatch", "expected_total_cents does not match the total computed from line_items")
	ErrAmountOverflow = apperr.New(http.StatusUnprocessableEntity, "amount_overflow", "line item amount or invoice total exceeds the supported range")
)
