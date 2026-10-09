package invoices

import (
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/httpx"
)

const dateLayout = "2006-01-02"

type createRequest struct {
	CustomerID         string            `json:"customer_id"`
	DueDate            string            `json:"due_date"`
	LineItems          []lineItemRequest `json:"line_items"`
	ExpectedTotalCents int64             `json:"expected_total_cents"`
}

type lineItemRequest struct {
	Description     string `json:"description"`
	Quantity        int64  `json:"quantity"`
	UnitAmountCents int64  `json:"unit_amount_cents"`
}

type lineItemResponse struct {
	Description     string `json:"description"`
	Quantity        int64  `json:"quantity"`
	UnitAmountCents int64  `json:"unit_amount_cents"`
	AmountCents     int64  `json:"amount_cents"`
}

type invoiceResponse struct {
	ID                    uuid.UUID          `json:"id"`
	InvoiceSequenceNumber *string            `json:"invoice_sequence_number"`
	CustomerID            uuid.UUID          `json:"customer_id"`
	Status                Status             `json:"status"`
	Currency              string             `json:"currency"`
	TotalCents            int64              `json:"total_cents"`
	DueDate               string             `json:"due_date"`
	LineItems             []lineItemResponse `json:"line_items"`
	PaidAt                *time.Time         `json:"paid_at"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

// toCreateInput parses the request's string fields. Range and business rules
// are validated by the service.
func toCreateInput(req createRequest) (CreateInput, error) {
	customerID, err := httpx.ParseUUID(req.CustomerID)
	if err != nil {
		return CreateInput{}, apperr.Validation("customer_id", "must be a UUID")
	}
	due, err := time.Parse(dateLayout, req.DueDate)
	if err != nil {
		return CreateInput{}, apperr.Validation("due_date", "must be a date in YYYY-MM-DD format")
	}
	in := CreateInput{
		CustomerID:         customerID,
		DueDate:            due,
		ExpectedTotalCents: req.ExpectedTotalCents,
		Lines:              make([]LineInput, len(req.LineItems)),
	}
	for i, l := range req.LineItems {
		in.Lines[i] = LineInput(l)
	}
	return in, nil
}

func toResponse(inv Invoice) invoiceResponse {
	items := make([]lineItemResponse, len(inv.LineItems)) // non-nil so it encodes as []
	for i, l := range inv.LineItems {
		items[i] = lineItemResponse(l)
	}
	return invoiceResponse{
		ID:                    inv.ID,
		InvoiceSequenceNumber: inv.SequenceNumber,
		CustomerID:            inv.CustomerID,
		Status:                inv.Status,
		Currency:              inv.Currency,
		TotalCents:            inv.TotalCents,
		DueDate:               inv.DueDate.Format(dateLayout),
		LineItems:             items,
		PaidAt:                inv.PaidAt,
		CreatedAt:             inv.CreatedAt,
		UpdatedAt:             inv.UpdatedAt,
	}
}

func toResponses(invs []Invoice) []invoiceResponse {
	out := make([]invoiceResponse, len(invs))
	for i, inv := range invs {
		out[i] = toResponse(inv)
	}
	return out
}

type payRequest struct {
	CardToken   string `json:"card_token"`
	AmountCents int64  `json:"amount_cents"`
}

type attemptResponse struct {
	ID          uuid.UUID     `json:"id"`
	InvoiceID   uuid.UUID     `json:"invoice_id"`
	AmountCents int64         `json:"amount_cents"`
	Status      AttemptStatus `json:"status"`
	FailureCode *string       `json:"failure_code"`
	PSPRefID    *string       `json:"psp_ref_id"`
	CreatedAt   time.Time     `json:"created_at"`
	ResolvedAt  *time.Time    `json:"resolved_at"`
}

func toAttemptResponse(a Attempt) attemptResponse {
	return attemptResponse{
		ID: a.ID, InvoiceID: a.InvoiceID, AmountCents: a.AmountCents, Status: a.Status,
		FailureCode: a.FailureCode, PSPRefID: a.PSPRefID, CreatedAt: a.CreatedAt, ResolvedAt: a.ResolvedAt,
	}
}
