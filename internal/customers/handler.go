package customers

import (
	"net/http"

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
	c, err := h.svc.Create(r.Context(), p.BusinessID, req.Name, req.Email)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, toResponse(c))
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
	c, err := h.svc.Get(r.Context(), p.BusinessID, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toResponse(c))
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
	cs, hasMore, err := h.svc.List(r.Context(), p.BusinessID, page.After, page.Limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, httpx.Page[customerResponse]{Data: toResponses(cs), HasMore: hasMore})
}
