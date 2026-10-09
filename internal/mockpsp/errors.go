package mockpsp

import (
	"net/http"

	"invoice-and-payment-service/internal/apperr"
)

var (
	ErrNotFound            = apperr.New(http.StatusNotFound, "charge_not_found", "no charge for that idempotency key")
	ErrKeyRequired         = apperr.New(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
	ErrIdempotencyConflict = apperr.New(http.StatusConflict, "idempotency_conflict", "this idempotency key was already used with different parameters")
)
