package auth

import (
	"errors"
	"net/http"

	"invoice-and-payment-service/internal/apperr"
)

// ErrUnauthorized is the single response for a missing, malformed, unknown or
// revoked key, so a caller can't tell them apart.
var ErrUnauthorized = apperr.New(http.StatusUnauthorized, "unauthorized", "missing or invalid API key")

var errKeyNotFound = errors.New("api key not found")
