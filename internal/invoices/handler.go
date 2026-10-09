package invoices

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/auth"
	"invoice-and-payment-service/internal/httpx"
)

type handler struct {
	svc *Service
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	in, err := toCreateInput(req)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	inv, err := h.svc.Create(r.Context(), p.BusinessID, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, toResponse(inv))
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathUUID(r, "id", ErrNotFound)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	inv, err := h.svc.Get(r.Context(), p.BusinessID, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toResponse(inv))
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	status, err := parseStatusFilter(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	invs, hasMore, err := h.svc.List(r.Context(), p.BusinessID, status, page.After, page.Limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, httpx.Page[invoiceResponse]{Data: toResponses(invs), HasMore: hasMore})
}

// parseStatusFilter reads the optional ?status= parameter.
func parseStatusFilter(r *http.Request) (*Status, error) {
	v := r.URL.Query().Get("status")
	if v == "" {
		return nil, nil
	}
	s := Status(v)
	if !s.valid() {
		names := make([]string, len(allStatuses))
		for i, st := range allStatuses {
			names[i] = string(st)
		}
		return nil, apperr.Validation("status", fmt.Sprintf("must be one of %s", strings.Join(names, ", ")))
	}
	return &s, nil
}

// transition builds a handler for one of the body-less status-change endpoints.
func (h *handler) transition(do func(*Service, context.Context, uuid.UUID, uuid.UUID) (Invoice, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.Require(w, r)
		if !ok {
			return
		}
		id, err := httpx.PathUUID(r, "id", ErrNotFound)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		inv, err := do(h.svc, r.Context(), p.BusinessID, id)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, r, http.StatusOK, toResponse(inv))
	}
}

const maxIdempotencyKeyLen = 255

// pay validates the request before anything touches the database, so a bad
// request never takes the invoice lock: header, then body, then the invoice.
func (h *handler) pay(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		httpx.WriteError(w, r, ErrKeyRequired)
		return
	}
	if utf8.RuneCountInString(key) > maxIdempotencyKeyLen {
		httpx.WriteError(w, r, apperr.Validation("Idempotency-Key", "must be at most 255 characters"))
		return
	}
	var req payRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id", ErrNotFound)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	attempt, err := h.svc.Pay(r.Context(), p.BusinessID, PayInput{
		InvoiceID: id, IdempotencyKey: key, CardToken: req.CardToken, AmountCents: req.AmountCents,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// 200 once the provider has answered; 202 while the outcome is still unknown.
	status := http.StatusOK
	if !attempt.Resolved() {
		status = http.StatusAccepted
	}
	httpx.WriteJSON(w, r, status, toAttemptResponse(attempt))
}

func (h *handler) getAttempt(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathUUID(r, "id", ErrAttemptNotFound)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a, err := h.svc.GetAttempt(r.Context(), p.BusinessID, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toAttemptResponse(a))
}

func (h *handler) listAttempts(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	id, err := httpx.PathUUID(r, "id", ErrNotFound)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	attempts, err := h.svc.ListAttempts(r.Context(), p.BusinessID, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]attemptResponse, len(attempts))
	for i, a := range attempts {
		out[i] = toAttemptResponse(a)
	}
	httpx.WriteJSON(w, r, http.StatusOK, struct {
		Data []attemptResponse `json:"data"`
	}{out})
}
