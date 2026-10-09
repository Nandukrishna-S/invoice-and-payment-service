package mockpsp

import (
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/httpx"
)

const (
	maxKeyLen   = 255
	maxTokenLen = 100
)

type handler struct {
	svc *Service
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		httpx.WriteError(w, r, ErrKeyRequired)
		return
	}
	if utf8.RuneCountInString(key) > maxKeyLen {
		httpx.WriteError(w, r, apperr.Validation("Idempotency-Key", "must be at most 255 characters"))
		return
	}
	var req chargeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	switch {
	case req.Token == "" || utf8.RuneCountInString(req.Token) > maxTokenLen:
		httpx.WriteError(w, r, apperr.Validation("token", "is required and must be at most 100 characters"))
		return
	case req.AmountCents < 1:
		httpx.WriteError(w, r, apperr.Validation("amount_cents", "must be at least 1"))
		return
	}

	res, err := h.svc.Charge(r.Context(), key, req.Token, req.AmountCents)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if res.DropConnection {
		// The charge is stored; hang up without a response. net/http closes the
		// connection quietly on this sentinel.
		panic(http.ErrAbortHandler)
	}
	httpx.WriteJSON(w, r, http.StatusOK, toResponse(res.Charge))
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.Get(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, toResponse(c))
}
