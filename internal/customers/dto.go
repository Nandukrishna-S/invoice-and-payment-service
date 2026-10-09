package customers

import (
	"time"

	"github.com/google/uuid"
)

type createRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type customerResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func toResponse(c Customer) customerResponse {
	return customerResponse{ID: c.ID, Name: c.Name, Email: c.Email, CreatedAt: c.CreatedAt}
}

func toResponses(cs []Customer) []customerResponse {
	out := make([]customerResponse, len(cs)) // non-nil so an empty page encodes as []
	for i, c := range cs {
		out[i] = toResponse(c)
	}
	return out
}
