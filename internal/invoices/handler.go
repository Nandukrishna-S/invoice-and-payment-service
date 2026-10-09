package invoices

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
