package webhooks

import (
	"net/http"

	"invoice-and-payment-service/internal/auth"
	"invoice-and-payment-service/internal/httpx"
)

type handler struct {
	svc *Service
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	e, err := h.svc.Register(r.Context(), p.BusinessID, req.URL)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// The only response that ever shows the secret.
	httpx.WriteJSON(w, r, http.StatusCreated, registeredResponse{endpointResponse: toResponse(e), Secret: e.Secret})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.Require(w, r)
	if !ok {
		return
	}
	endpoints, err := h.svc.List(r.Context(), p.BusinessID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]endpointResponse, len(endpoints))
	for i, e := range endpoints {
		out[i] = toResponse(e)
	}
	httpx.WriteJSON(w, r, http.StatusOK, struct {
		Data []endpointResponse `json:"data"`
	}{out})
}
