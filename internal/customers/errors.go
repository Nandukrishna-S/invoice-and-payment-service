package customers

import (
	"net/http"

	"invoice-and-payment-service/internal/apperr"
)

var ErrNotFound = apperr.New(http.StatusNotFound, "customer_not_found", "customer not found")
