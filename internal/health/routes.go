package health

import "github.com/go-chi/chi/v5"

func RegisterRoutes(r chi.Router, db Pinger) {
	h := &Handler{db: db}
	r.Get("/healthz", h.Healthz)
}
