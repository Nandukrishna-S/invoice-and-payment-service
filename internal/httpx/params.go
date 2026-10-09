package httpx

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
)

var errNotCanonicalUUID = errors.New("not a canonical UUID")

const (
	defaultLimit = 20
	maxLimit     = 100
)

// Page is the envelope every list endpoint returns.
type Page[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// PageParams are the validated limit and starting_after query parameters.
type PageParams struct {
	Limit int
	After *uuid.UUID
}

func ParsePage(r *http.Request) (PageParams, error) {
	p := PageParams{Limit: defaultLimit}
	q := r.URL.Query()

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return PageParams{}, apperr.Validation("limit", "must be an integer between 1 and 100")
		}
		p.Limit = n
	}
	if v := q.Get("starting_after"); v != "" {
		id, err := parseUUID(v)
		if err != nil {
			return PageParams{}, apperr.Validation("starting_after", "must be a UUID")
		}
		p.After = &id
	}
	return p, nil
}

// PathUUID reads a UUID path parameter. A malformed value returns notFound,
// because no resource can have that ID.
func PathUUID(r *http.Request, name string, notFound *apperr.Error) (uuid.UUID, error) {
	id, err := parseUUID(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, notFound
	}
	return id, nil
}

// parseUUID accepts only the canonical 36-character form; uuid.Parse alone also
// takes braces, urn: prefixes and bare hex.
func parseUUID(s string) (uuid.UUID, error) {
	if len(s) != 36 {
		return uuid.Nil, errNotCanonicalUUID
	}
	return uuid.Parse(s)
}
