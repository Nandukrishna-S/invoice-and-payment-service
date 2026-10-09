package webhooks

import (
	"context"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const endpointColumns = `id, business_id, url, secret, created_at, disabled_at`

func (repo) insertEndpoint(ctx context.Context, q db.Querier, e Endpoint) (Endpoint, error) {
	err := q.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (id, business_id, url, secret) VALUES ($1, $2, $3, $4)
		 RETURNING created_at, disabled_at`,
		e.ID, e.BusinessID, e.URL, e.Secret).Scan(&e.CreatedAt, &e.DisabledAt)
	return e, err
}

// listEndpoints returns every endpoint of the business, newest first.
func (repo) listEndpoints(ctx context.Context, q db.Querier, businessID uuid.UUID) ([]Endpoint, error) {
	rows, err := q.Query(ctx,
		`SELECT `+endpointColumns+` FROM webhook_endpoints WHERE business_id = $1 ORDER BY id DESC`, businessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.BusinessID, &e.URL, &e.Secret, &e.CreatedAt, &e.DisabledAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// activeEndpointIDs returns the endpoints that should receive a new event.
func (repo) activeEndpointIDs(ctx context.Context, q db.Querier, businessID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx,
		`SELECT id FROM webhook_endpoints WHERE business_id = $1 AND disabled_at IS NULL ORDER BY id`, businessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// insertDeliveries queues the same event for several endpoints in one statement.
func (repo) insertDeliveries(ctx context.Context, q db.Querier, eventID, eventType string, payload []byte, endpointIDs []uuid.UUID) error {
	ids := make([]uuid.UUID, len(endpointIDs))
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}
	_, err := q.Exec(ctx,
		`INSERT INTO webhook_deliveries (id, event_id, endpoint_id, type, payload)
		 SELECT d.id, $1, d.endpoint_id, $2, $3::jsonb
		 FROM unnest($4::uuid[], $5::uuid[]) AS d(id, endpoint_id)`,
		eventID, eventType, payload, ids, endpointIDs)
	return err
}
