package auth

import (
	"context"

	"github.com/google/uuid"
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
