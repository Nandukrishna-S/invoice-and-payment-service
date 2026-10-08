// Package health serves the liveness and database check.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
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

	status, body := http.StatusOK, "ok"
	if err := h.db.Ping(ctx); err != nil {
		slog.ErrorContext(r.Context(), "healthz: database ping failed", "error", err)
		status, body = http.StatusServiceUnavailable, "unavailable"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": body})
}
