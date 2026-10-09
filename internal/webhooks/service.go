package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/db"
)

const maxURLLen = 2048

type Service struct {
	db   db.Querier
	repo repo
}

func NewService(q db.Querier) *Service { return &Service{db: q} }

// Register creates an endpoint with a fresh signing secret.
func (s *Service) Register(ctx context.Context, businessID uuid.UUID, rawURL string) (Endpoint, error) {
	rawURL = strings.TrimSpace(rawURL)
	if err := validateURL(rawURL); err != nil {
		return Endpoint{}, err
	}
	secret, err := NewSecret()
	if err != nil {
		return Endpoint{}, err
	}
	return s.repo.insertEndpoint(ctx, s.db, Endpoint{
		ID: uuid.Must(uuid.NewV7()), BusinessID: businessID, URL: rawURL, Secret: secret,
	})
}

func (s *Service) List(ctx context.Context, businessID uuid.UUID) ([]Endpoint, error) {
	return s.repo.listEndpoints(ctx, s.db, businessID)
}

// validateURL accepts absolute http and https URLs. Plain http is allowed so the
// demo receiver works; production should require https (DESIGN section 7).
func validateURL(raw string) error {
	switch {
	case raw == "":
		return apperr.Validation("url", "is required")
	case utf8.RuneCountInString(raw) > maxURLLen:
		return apperr.Validation("url", "must be at most 2048 characters")
	case strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0:
		return apperr.Validation("url", "must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return apperr.Validation("url", "must be an absolute http or https URL")
	}
	if u.User != nil {
		// Credentials in a URL would end up in logs and in the stored endpoint.
		return apperr.Validation("url", "must not contain credentials")
	}
	return nil
}

// Outbox queues events for delivery. It is a separate type from Service because
// it works on the caller's transaction, not on its own connection.
type Outbox struct {
	repo repo
	// firstAttemptDelay is the retry schedule's first entry: how long a new
	// delivery waits before its first attempt (normally none).
	firstAttemptDelay time.Duration
}

func NewOutbox(firstAttemptDelay time.Duration) *Outbox {
	return &Outbox{firstAttemptDelay: firstAttemptDelay}
}

// Enqueue queues one event for every active endpoint of the business, using the
// caller's transaction q, so the event exists if and only if the surrounding
// change commits. All rows of the event share one event id (the webhook-id
// header) and retry independently.
//
// snapshot builds the event's object (an invoice as GET /invoices/{id} shows
// it). It runs only when somebody is listening, so a business with no endpoints
// pays one cheap query. Endpoints registered later never see past events.
func (o *Outbox) Enqueue(ctx context.Context, q db.Querier, businessID uuid.UUID, eventType string, at time.Time, snapshot func(context.Context) (any, error)) error {
	endpoints, err := o.repo.activeEndpointIDs(ctx, q, businessID)
	if err != nil {
		return fmt.Errorf("find webhook endpoints: %w", err)
	}
	if len(endpoints) == 0 {
		return nil
	}
	object, err := snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", eventType, err)
	}
	eventID := "evt_" + uuid.Must(uuid.NewV7()).String()
	payload, err := json.Marshal(event{ID: eventID, Type: eventType, CreatedAt: at.UTC(), Data: eventData{Object: object}})
	if err != nil {
		return fmt.Errorf("encode %s: %w", eventType, err)
	}
	if err := o.repo.insertDeliveries(ctx, q, eventID, eventType, payload, endpoints, o.firstAttemptDelay); err != nil {
		return fmt.Errorf("queue %s: %w", eventType, err)
	}
	return nil
}
