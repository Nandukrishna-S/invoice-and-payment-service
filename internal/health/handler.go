// Package health serves the liveness and database check.
package health

import (
	"context"
	"net/http"
	"time"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/httpx"
)

// Pinger is satisfied by *pgxpool.Pool; it exists so the handler can be tested
// without a database.
type Pinger interface {
	Ping(ctx context.Context) error
}

const pingTimeout = 2 * time.Second

type Handler struct {
	db Pinger
}

func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		httpx.WriteError(w, r, apperr.ErrServiceUnavailable.Wrap(err))
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
