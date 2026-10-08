package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"invoice-and-payment-service/internal/httpx"
)

// Middleware authenticates the Bearer key and stores the Principal in the
// request context, adding business_id and api_key_id to the request's log fields.
func Middleware(s *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r)
			if !ok {
				unauthorized(w, r)
				return
			}
			p, err := s.Authenticate(r.Context(), key)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					unauthorized(w, r)
					return
				}
				httpx.WriteError(w, r, err)
				return
			}
			ctx := WithPrincipal(r.Context(), p)
			ctx = httpx.WithLogAttrs(ctx,
				slog.String("business_id", p.BusinessID.String()),
				slog.String("api_key_id", p.APIKeyID.String()))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.WriteError(w, r, ErrUnauthorized)
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}
