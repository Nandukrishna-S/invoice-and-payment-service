package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/apperr"
)

type payload struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

func decode(t *testing.T, contentType, body string) (payload, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	var p payload
	err := DecodeJSON(httptest.NewRecorder(), req, &p)
	return p, err
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    string // "" means success
		wantStatus  int
	}{
		{"valid", "application/json", `{"name":"a","amount":5}`, "", 0},
		{"valid with charset", "application/json; charset=utf-8", `{"name":"a"}`, "", 0},
		{"missing content type", "", `{"name":"a"}`, "unsupported_media_type", 415},
		{"wrong content type", "text/plain", `{"name":"a"}`, "unsupported_media_type", 415},
		{"malformed json", "application/json", `{"name":`, "invalid_json", 400},
		{"empty body", "application/json", ``, "invalid_json", 400},
		{"not an object", "application/json", `[1,2]`, "validation_failed", 422},
		{"unknown field", "application/json", `{"name":"a","business_id":"x"}`, "unknown_field", 400},
		{"wrong type", "application/json", `{"name":"a","amount":"5"}`, "validation_failed", 422},
		{"float for integer", "application/json", `{"amount":1.5}`, "validation_failed", 422},
		{"int64 overflow", "application/json", `{"amount":99999999999999999999}`, "validation_failed", 422},
		{"two documents", "application/json", `{"name":"a"}{"name":"b"}`, "invalid_json", 400},
		{"trailing garbage", "application/json", `{"name":"a"} xyz`, "invalid_json", 400},
		{"too large", "application/json", `{"name":"` + strings.Repeat("a", 1<<20) + `"}`, "request_too_large", 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decode(t, tt.contentType, tt.body)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("want *apperr.Error, got %T %v", err, err)
			}
			if ae.Code != tt.wantCode || ae.Status != tt.wantStatus {
				t.Fatalf("got %d/%s, want %d/%s", ae.Status, ae.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestDecodeJSONNamesTheField(t *testing.T) {
	_, err := decode(t, "application/json", `{"amount":"x"}`)
	if !strings.Contains(err.Error(), "amount") {
		t.Fatalf("type error %q should name the field", err)
	}
	_, err = decode(t, "application/json", `{"bogus":1}`)
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown-field error %q should name the field", err)
	}
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) (code, message, requestID string) {
	t.Helper()
	var e struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not the error envelope: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	return e.Error.Code, e.Error.Message, e.Error.RequestID
}

func TestWriteError(t *testing.T) {
	t.Run("apperr is sent as is", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), apperr.Validation("limit", "too big"))
		code, msg, _ := envelope(t, rec)
		if rec.Code != 422 || code != "validation_failed" || !strings.Contains(msg, "limit") {
			t.Fatalf("got %d %s %q", rec.Code, code, msg)
		}
	})
	t.Run("wrapped apperr is found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		err := errors.Join(errors.New("ctx"), apperr.ErrRouteNotFound)
		WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), err)
		if code, _, _ := envelope(t, rec); rec.Code != 404 || code != "route_not_found" {
			t.Fatalf("got %d %s", rec.Code, code)
		}
	})
	t.Run("unknown error is a 500 that leaks nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("pq: password=hunter2"))
		code, msg, _ := envelope(t, rec)
		if rec.Code != 500 || code != "internal_error" {
			t.Fatalf("got %d %s", rec.Code, code)
		}
		if strings.Contains(msg, "hunter2") || strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatal("internal error text leaked to the client")
		}
	})
}

func router() chi.Router {
	r := chi.NewRouter()
	r.Use(RequestID, Recoverer)
	r.NotFound(NotFound)
	r.MethodNotAllowed(MethodNotAllowed)
	r.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, map[string]string{"id": RequestIDFrom(r.Context())})
	})
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	return r
}

func TestUnmatchedRoutesUseTheEnvelope(t *testing.T) {
	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/nope", 404, "route_not_found"},
		{http.MethodPost, "/ok", 405, "method_not_allowed"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		router().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		code, _, reqID := envelope(t, rec)
		if rec.Code != tt.status || code != tt.code {
			t.Errorf("%s %s: got %d/%s, want %d/%s", tt.method, tt.path, rec.Code, code, tt.status, tt.code)
		}
		if reqID == "" || reqID != rec.Header().Get("X-Request-ID") {
			t.Errorf("%s %s: envelope request_id %q must match header %q", tt.method, tt.path, reqID, rec.Header().Get("X-Request-ID"))
		}
	}
}

func TestRequestID(t *testing.T) {
	get := func(header string) string {
		req := httptest.NewRequest(http.MethodGet, "/ok", nil)
		if header != "" {
			req.Header.Set("X-Request-ID", header)
		}
		rec := httptest.NewRecorder()
		router().ServeHTTP(rec, req)
		return rec.Header().Get("X-Request-ID")
	}

	if id := get(""); id == "" {
		t.Error("an ID should be generated when none is supplied")
	}
	if id := get("abc-123_X"); id != "abc-123_X" {
		t.Errorf("a safe caller ID should be kept, got %q", id)
	}
	for _, bad := range []string{"has space", "inj\"ect", strings.Repeat("a", 65)} {
		if id := get(bad); id == bad || id == "" {
			t.Errorf("unsafe ID %q must be replaced, got %q", bad, id)
		}
	}
	if a, b := get(""), get(""); a == b {
		t.Error("generated IDs must be unique")
	}
}

func TestRecovererReturnsEnvelopeAndLogsPanic(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	defer slog.SetDefault(prev)

	rec := httptest.NewRecorder()
	router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	code, msg, reqID := envelope(t, rec)
	if rec.Code != 500 || code != "internal_error" {
		t.Fatalf("got %d %s", rec.Code, code)
	}
	if strings.Contains(msg, "kaboom") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatal("panic details leaked to the client")
	}
	out := logs.String()
	if !strings.Contains(out, "kaboom") || !strings.Contains(out, reqID) {
		t.Fatalf("log should carry the panic value and request_id %q: %s", reqID, out)
	}
}

func TestRecovererRepanicsOnAbortHandler(t *testing.T) {
	h := Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("http.ErrAbortHandler must propagate")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestContextHandlerAddsAttrsAndSurvivesDerivedLoggers(t *testing.T) {
	var logs bytes.Buffer
	l := slog.New(NewContextHandler(slog.NewJSONHandler(&logs, nil)))

	ctx := WithLogAttrs(context.Background(), slog.String("request_id", "r1"))
	ctx = WithLogAttrs(ctx, slog.String("business_id", "b1"))

	l.With("component", "x").InfoContext(ctx, "hello")
	out := logs.String()
	for _, want := range []string{`"request_id":"r1"`, `"business_id":"b1"`, `"component":"x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %s is missing %s", out, want)
		}
	}

	logs.Reset()
	l.WithGroup("g").InfoContext(ctx, "grouped")
	if !strings.Contains(logs.String(), "r1") {
		t.Errorf("attrs lost after WithGroup: %s", logs.String())
	}
	logs.Reset()
	l.Info("no ctx attrs")
	if strings.Contains(logs.String(), "request_id") {
		t.Errorf("attrs leaked into a context without them: %s", logs.String())
	}
}
