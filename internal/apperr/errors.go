package apperr

import (
	"fmt"
	"net/http"
)

// Errors that can occur on any endpoint (see the table in openapi.yaml).
// Resource-specific errors live in each module's errors.go.
var (
	ErrInvalidJSON          = New(http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
	ErrRouteNotFound        = New(http.StatusNotFound, "route_not_found", "no such route")
	ErrMethodNotAllowed     = New(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this route")
	ErrRequestTooLarge      = New(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 1 MiB")
	ErrUnsupportedMediaType = New(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	ErrInternal             = New(http.StatusInternalServerError, "internal_error", "internal error")
	ErrServiceUnavailable   = New(http.StatusServiceUnavailable, "service_unavailable", "service unavailable")
)

// UnknownField is returned when the body names a field the schema doesn't have.
func UnknownField(name string) *Error {
	return New(http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", name))
}

// Validation is a 422 that names the offending field or parameter.
func Validation(field, problem string) *Error {
	return New(http.StatusUnprocessableEntity, "validation_failed", fmt.Sprintf("%s: %s", field, problem))
}
