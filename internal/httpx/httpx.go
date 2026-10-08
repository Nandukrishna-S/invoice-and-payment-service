// Package httpx holds the HTTP plumbing shared by all modules: JSON in/out and
// the error envelope.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"invoice-and-payment-service/internal/apperr"
)

const maxBodyBytes = 1 << 20 // 1 MiB

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.ErrorContext(r.Context(), "write response", "error", err)
	}
}

// WriteError is the only way handlers report failures. An *apperr.Error is sent
// as is; anything else becomes a 500 whose cause is logged, never exposed.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		ae = apperr.ErrInternal.Wrap(err)
	}
	if ae.Status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed", "code", ae.Code, "error", ae.Err)
	}
	WriteJSON(w, r, ae.Status, errorEnvelope{Error: errorBody{
		Code:      ae.Code,
		Message:   ae.Message,
		RequestID: RequestIDFrom(r.Context()),
	}})
}

// DecodeJSON reads the request body into dst. It requires a JSON Content-Type,
// caps the body at 1 MiB, rejects unknown fields and trailing data, and returns
// an *apperr.Error for every failure.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return apperr.ErrUnsupportedMediaType
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value after the first (or garbage) means the body isn't one JSON document.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return decodeError(err)
		}
		return apperr.ErrInvalidJSON.WithMessage("request body must contain a single JSON object")
	}
	return nil
}

func decodeError(err error) *apperr.Error {
	var tooLarge *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return apperr.ErrRequestTooLarge
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apperr.Validation(field, "has the wrong type")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json exposes no typed error for this case.
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apperr.UnknownField(name)
	default:
		return apperr.ErrInvalidJSON.Wrap(err)
	}
}
