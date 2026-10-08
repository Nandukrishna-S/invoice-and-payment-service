package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{"database reachable", nil, http.StatusOK, `"status":"ok"`},
		{"database down", errors.New("connection refused"), http.StatusServiceUnavailable, `"status":"unavailable"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			RegisterRoutes(r, fakePinger{err: tt.pingErr})

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want it to contain %s", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
