package webhooks

import (
	"time"

	"github.com/google/uuid"
)

type registerRequest struct {
	URL string `json:"url"`
}

type endpointResponse struct {
	ID         uuid.UUID  `json:"id"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

// registeredResponse is the one response that carries the secret.
type registeredResponse struct {
	endpointResponse
	Secret string `json:"secret"`
}

func toResponse(e Endpoint) endpointResponse {
	return endpointResponse{ID: e.ID, URL: e.URL, CreatedAt: e.CreatedAt, DisabledAt: e.DisabledAt}
}

// event is the body delivered to an endpoint (the WebhookEvent schema).
type event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      eventData `json:"data"`
}

type eventData struct {
	Object any `json:"object"`
}
