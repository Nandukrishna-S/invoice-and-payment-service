package mockpsp

import "time"

type chargeRequest struct {
	Token       string `json:"token"`
	AmountCents int64  `json:"amount_cents"`
}

type chargeResponse struct {
	IdempotencyKey string     `json:"idempotency_key"`
	PSPRefID       string     `json:"psp_ref_id"`
	AmountCents    int64      `json:"amount_cents"`
	Status         string     `json:"status"`
	FailureCode    *string    `json:"failure_code"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

func toResponse(c Charge) chargeResponse {
	return chargeResponse{
		IdempotencyKey: c.IdempotencyKey,
		PSPRefID:       c.PSPRefID,
		AmountCents:    c.AmountCents,
		Status:         c.Status,
		FailureCode:    c.FailureCode,
		CreatedAt:      c.CreatedAt,
		CompletedAt:    c.CompletedAt,
	}
}
