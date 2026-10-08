package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
)

const requestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// A caller-supplied ID is only trusted if it is short and made of safe
// characters, since it ends up in logs and response bodies.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// RequestIDFrom returns the ID set by RequestID, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID assigns every request an ID, echoes it in the response header, and
// adds it to the request's log fields.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set(requestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx = WithLogAttrs(ctx, slog.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recoverer turns a handler panic into a 500 envelope and logs the stack.
// It must sit inside RequestID so the response carries the request ID.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec) // net/http's own signal to abort the response
			}
			slog.ErrorContext(r.Context(), "panic recovered", "panic", rec, "stack", string(debug.Stack()))
			WriteError(w, r, apperr.ErrInternal)
		}()
		next.ServeHTTP(w, r)
	})
}

// NotFound and MethodNotAllowed give unmatched routes the standard envelope.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, apperr.ErrRouteNotFound)
}

func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, apperr.ErrMethodNotAllowed)
}
