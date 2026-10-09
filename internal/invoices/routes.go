package invoices

import (
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func RegisterRoutes(r chi.Router, pool *pgxpool.Pool) {
	h := &handler{svc: NewService(pool)}
	r.Post("/invoices", h.create)
	r.Get("/invoices", h.list)
	r.Get("/invoices/{id}", h.get)
}
