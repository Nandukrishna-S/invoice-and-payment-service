package auth

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/httpx"
)

// Principal is the authenticated caller. BusinessID comes only from the API
// key, never from the request.
type Principal struct {
	BusinessID uuid.UUID
	APIKeyID   uuid.UUID
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the caller set by Middleware. ok is false outside an
// authenticated route, which is a wiring bug in the caller.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Require returns the caller for a handler. If there is none, the route was
// registered outside the auth group; it answers 500 and returns ok=false.
func Require(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.ErrInternal)
	}
	return p, ok
}
