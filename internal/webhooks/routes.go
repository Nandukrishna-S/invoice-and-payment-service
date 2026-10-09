package webhooks

import (
	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/db"
)

func RegisterRoutes(r chi.Router, q db.Querier) {
	h := &handler{svc: NewService(q)}
	r.Post("/webhook_endpoints", h.register)
	r.Get("/webhook_endpoints", h.list)
}
