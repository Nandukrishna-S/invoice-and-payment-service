package customers

import (
	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/db"
)

func RegisterRoutes(r chi.Router, q db.Querier) {
	h := &handler{svc: NewService(q)}
	r.Post("/customers", h.create)
	r.Get("/customers", h.list)
	r.Get("/customers/{id}", h.get)
}
