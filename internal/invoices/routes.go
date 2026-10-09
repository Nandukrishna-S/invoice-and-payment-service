package invoices

import (
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RegisterRoutes mounts the invoice and payment endpoints. fiscalLoc is the
// timezone in which the April-to-March financial year boundary is judged, and
// psp is the payment provider the pay flow charges.
func RegisterRoutes(r chi.Router, pool *pgxpool.Pool, fiscalLoc *time.Location, psp PSPClient) {
	registerRoutes(r, NewService(pool, fiscalLoc, psp))
}

func registerRoutes(r chi.Router, svc *Service) {
	h := &handler{svc: svc}
	r.Post("/invoices", h.create)
	r.Get("/invoices", h.list)
	r.Get("/invoices/{id}", h.get)
	r.Post("/invoices/{id}/finalize", h.transition((*Service).Finalize))
	r.Post("/invoices/{id}/void", h.transition((*Service).Void))
	r.Post("/invoices/{id}/mark_uncollectible", h.transition((*Service).MarkUncollectible))
	r.Post("/invoices/{id}/pay", h.pay)
	r.Get("/invoices/{id}/payment_attempts", h.listAttempts)
	r.Get("/payment_attempts/{id}", h.getAttempt)
}
