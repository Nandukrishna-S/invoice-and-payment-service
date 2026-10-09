package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"invoice-and-payment-service/internal/apperr"
)

func TestParsePage(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name      string
		query     string
		wantLimit int
		wantAfter bool
		wantField string // "" means valid
	}{
		{"defaults", "", 20, false, ""},
		{"explicit limit", "limit=100", 100, false, ""},
		{"minimum limit", "limit=1", 1, false, ""},
		{"cursor", "starting_after=" + id.String(), 20, true, ""},
		{"limit zero", "limit=0", 0, false, "limit"},
		{"limit too big", "limit=101", 0, false, "limit"},
		{"limit negative", "limit=-5", 0, false, "limit"},
		{"limit not a number", "limit=ten", 0, false, "limit"},
		{"cursor not a uuid", "starting_after=abc", 0, false, "starting_after"},
		{"cursor without hyphens", "starting_after=" + strings.ReplaceAll(id.String(), "-", ""), 0, false, "starting_after"},
		{"cursor with braces", "starting_after={" + id.String() + "}", 0, false, "starting_after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePage(httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil))
			if tt.wantField != "" {
				ae, ok := err.(*apperr.Error)
				if !ok || ae.Code != "validation_failed" || !strings.Contains(ae.Message, tt.wantField) {
					t.Fatalf("want validation_failed naming %q, got %v", tt.wantField, err)
				}
				return
			}
			if err != nil || p.Limit != tt.wantLimit || (p.After != nil) != tt.wantAfter {
				t.Fatalf("got %+v, %v", p, err)
			}
		})
	}
}

func TestPathUUID(t *testing.T) {
	notFound := apperr.New(http.StatusNotFound, "thing_not_found", "thing not found")
	id := uuid.New()

	var got uuid.UUID
	var gotErr error
	r := chi.NewRouter()
	r.Get("/things/{id}", func(w http.ResponseWriter, req *http.Request) {
		got, gotErr = PathUUID(req, "id", notFound)
	})

	for path, wantOK := range map[string]bool{
		"/things/" + id.String():                              true,
		"/things/" + strings.ToUpper(id.String()):             true,
		"/things/not-a-uuid":                                  false,
		"/things/" + strings.ReplaceAll(id.String(), "-", ""): false,
		"/things/urn:uuid:" + id.String():                     false,
	} {
		got, gotErr = uuid.Nil, nil
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		if wantOK && (gotErr != nil || got != id) {
			t.Errorf("%s: got %v, %v", path, got, gotErr)
		}
		if !wantOK && gotErr != notFound {
			t.Errorf("%s: want the not-found error, got %v", path, gotErr)
		}
	}
}
