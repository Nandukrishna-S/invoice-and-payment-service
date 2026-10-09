// Package customers manages the people and companies a business invoices.
package customers

import (
	"time"

	"github.com/google/uuid"
)

type Customer struct {
	ID         uuid.UUID
	BusinessID uuid.UUID
	Name       string
	Email      string
	CreatedAt  time.Time
}
