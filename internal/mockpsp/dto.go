package mockpsp

import "time"

type chargeRequest struct {
	Token       string `json:"token"`
	AmountCents int64  `json:"amount_cents"`
}

type chargeResponse struct {
	IdempotencyKey string     `json:"idempotency_key"`
	PSPRef         string     `json:"psp_ref"`
	AmountCents    int64      `json:"amount_cents"`
	Status         string     `json:"status"`
	Code           *string    `json:"code"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

func toResponse(c Charge) chargeResponse {
	return chargeResponse{
		IdempotencyKey: c.IdempotencyKey,
		PSPRef:         c.PSPRefID,
		AmountCents:    c.AmountCents,
		Status:         c.Status,
		Code:           c.FailureCode,
		CreatedAt:      c.CreatedAt,
		CompletedAt:    c.CompletedAt,
	}
}
