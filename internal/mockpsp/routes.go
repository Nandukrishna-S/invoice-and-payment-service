package mockpsp

import "github.com/go-chi/chi/v5"

func RegisterRoutes(r chi.Router, svc *Service) {
	h := &handler{svc: svc}
	r.Post("/charges", h.create)
	r.Get("/charges/{key}", h.get)
}
