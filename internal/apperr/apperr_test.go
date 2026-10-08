package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWrapDoesNotMutateTheCatalogue(t *testing.T) {
	cause := errors.New("boom")
	wrapped := ErrInternal.Wrap(cause)

	if ErrInternal.Err != nil {
		t.Fatal("Wrap mutated the shared catalogue value")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("wrapped error should unwrap to its cause")
	}
	if !strings.Contains(wrapped.Error(), "boom") {
		t.Fatalf("Error() = %q, want the cause included", wrapped.Error())
	}
}

func TestWithMessageCopies(t *testing.T) {
	e := ErrRouteNotFound.WithMessage("custom")
	if e.Message != "custom" || ErrRouteNotFound.Message == "custom" {
		t.Fatalf("WithMessage must copy: got %q, original %q", e.Message, ErrRouteNotFound.Message)
	}
}

func TestErrorsAsThroughWrapping(t *testing.T) {
	err := fmt.Errorf("service: %w", Validation("limit", "must be between 1 and 100"))
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "validation_failed" || ae.Status != http.StatusUnprocessableEntity {
		t.Fatalf("errors.As failed: %+v", ae)
	}
	if !strings.Contains(ae.Message, "limit") {
		t.Fatalf("validation message %q should name the field", ae.Message)
	}
}

func TestCatalogueMatchesOpenAPI(t *testing.T) {
	tests := []struct {
		err    *Error
		status int
		code   string
	}{
		{ErrInvalidJSON, 400, "invalid_json"},
		{UnknownField("x"), 400, "unknown_field"},
		{ErrRouteNotFound, 404, "route_not_found"},
		{ErrMethodNotAllowed, 405, "method_not_allowed"},
		{ErrRequestTooLarge, 413, "request_too_large"},
		{ErrUnsupportedMediaType, 415, "unsupported_media_type"},
		{Validation("f", "p"), 422, "validation_failed"},
		{ErrInternal, 500, "internal_error"},
		{ErrServiceUnavailable, 503, "service_unavailable"},
	}
	for _, tt := range tests {
		if tt.err.Status != tt.status || tt.err.Code != tt.code {
			t.Errorf("%s: got %d/%s, want %d/%s", tt.code, tt.err.Status, tt.err.Code, tt.status, tt.code)
		}
	}
}
