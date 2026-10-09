// Package testapi runs a module's routes behind the same middleware as main,
// against a real migrated database, so integration tests go through auth.
package testapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/auth"
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/testdb"
)

type Env struct {
	Pool    *pgxpool.Pool
	Handler http.Handler
}

// New mounts register's routes inside the auth group, mirroring main.
func New(t testing.TB, register func(r chi.Router, pool *pgxpool.Pool)) *Env {
	t.Helper()
	pool := testdb.New(t)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(auth.NewService(pool)))
		register(r, pool)
	})
	return &Env{Pool: pool, Handler: r}
}

type Tenant struct {
	BusinessID uuid.UUID
	Key        string
}

// NewTenant inserts a business with one active API key.
func (e *Env) NewTenant(t testing.TB) Tenant {
	t.Helper()
	ctx := context.Background()
	key, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	business := uuid.Must(uuid.NewV7())
	if _, err := e.Pool.Exec(ctx, `INSERT INTO businesses (id, name) VALUES ($1, 'Test Business')`, business); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(ctx,
		`INSERT INTO api_keys (id, business_id, prefix, key_hash) VALUES ($1, $2, $3, $4)`,
		uuid.Must(uuid.NewV7()), business, key[:12], auth.HashKey(key)); err != nil {
		t.Fatal(err)
	}
	return Tenant{BusinessID: business, Key: key}
}

type Response struct {
	Code int
	Body []byte
}

// Do sends a request as the given API key ("" sends none). body may be a string
// (sent verbatim, for malformed-input tests), any value (JSON-encoded), or nil.
func (e *Env) Do(t testing.TB, key, method, path string, body any) Response {
	t.Helper()
	var reader *strings.Reader
	switch b := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	e.Handler.ServeHTTP(rec, req)
	return Response{Code: rec.Code, Body: rec.Body.Bytes()}
}

func (r Response) JSON(t testing.TB, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		t.Fatalf("response is not the expected JSON (%d): %s", r.Code, r.Body)
	}
}

// ErrorCode returns error.code from the standard envelope.
func (r Response) ErrorCode(t testing.TB) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	r.JSON(t, &e)
	return e.Error.Code
}

// ErrorMessage returns error.message from the standard envelope.
func (r Response) ErrorMessage(t testing.TB) string {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	r.JSON(t, &e)
	return e.Error.Message
}
