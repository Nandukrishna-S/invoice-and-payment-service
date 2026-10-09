package invoices

import "github.com/go-chi/chi/v5"

// RegisterRoutes mounts the invoice and payment endpoints. The service is built
// by the caller because the reconciler needs the very same one.
func RegisterRoutes(r chi.Router, svc *Service) {
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
