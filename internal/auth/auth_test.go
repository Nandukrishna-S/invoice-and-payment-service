package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/testdb"
)

const demoKey = "sk_test_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateKey()
	if !ValidKeyFormat(a) || a == b {
		t.Fatalf("keys must be well-formed and unique: %q %q", a, b)
	}
	if len(HashKey(a)) != 32 || !bytes.Equal(HashKey(a), HashKey(a)) || bytes.Equal(HashKey(a), HashKey(b)) {
		t.Fatal("HashKey must be deterministic, 32 bytes, and differ per key")
	}
	if displayPrefix(a) != a[:12] {
		t.Fatalf("display prefix = %q", displayPrefix(a))
	}
}

func TestValidKeyFormat(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{demoKey, true},
		{"", false},
		{"sk_test_abc", false},
		{demoKey + "0", false},
		{strings.ToUpper(demoKey), false},
		{"sk_live_" + demoKey[8:], false},
		{demoKey[:len(demoKey)-1] + "g", false},
	}
	for _, tt := range tests {
		if got := ValidKeyFormat(tt.key); got != tt.want {
			t.Errorf("ValidKeyFormat(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSeedGeneratesAKeyOnce(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService(pool)
	ctx := context.Background()

	key, err := svc.Seed(ctx, "")
	if err != nil || !ValidKeyFormat(key) {
		t.Fatalf("first seed: key=%q err=%v", key, err)
	}
	p, err := svc.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("generated key should authenticate: %v", err)
	}

	again, err := svc.Seed(ctx, "")
	if err != nil || again != "" {
		t.Fatalf("second seed must be a no-op: key=%q err=%v", again, err)
	}
	if countRows(t, pool, "businesses") != 1 || countRows(t, pool, "api_keys") != 1 {
		t.Fatal("seed must leave exactly one business and one key")
	}
	if p.BusinessID == uuid.Nil || p.APIKeyID == uuid.Nil {
		t.Fatal("principal ids must be set")
	}

	// Only the hash and a display prefix are stored.
	var stored int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE prefix = $1 AND key_hash = $2`,
		key[:12], HashKey(key)).Scan(&stored)
	if err != nil || stored != 1 {
		t.Fatalf("hash/prefix not stored as expected: %d %v", stored, err)
	}
}

func TestSeedUsesDemoKey(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService(pool)
	ctx := context.Background()

	generated, err := svc.Seed(ctx, demoKey)
	if err != nil || generated != "" {
		t.Fatalf("demo key must not be reported as generated: %q %v", generated, err)
	}
	if _, err := svc.Authenticate(ctx, demoKey); err != nil {
		t.Fatalf("demo key should authenticate: %v", err)
	}
}

func TestSeedRejectsMalformedDemoKeyAndCreatesNothing(t *testing.T) {
	pool := testdb.New(t)
	if _, err := NewService(pool).Seed(context.Background(), "not-a-key"); err == nil {
		t.Fatal("expected an error")
	}
	if countRows(t, pool, "businesses") != 0 {
		t.Fatal("nothing may be created for a bad demo key")
	}
}

func TestConcurrentSeedCreatesOneBusiness(t *testing.T) {
	pool := testdb.New(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := NewService(pool).Seed(context.Background(), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := countRows(t, pool, "businesses"); n != 1 {
		t.Fatalf("got %d businesses, want 1", n)
	}
}

func TestAuthenticateRejectsUnknownMalformedAndRevoked(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService(pool)
	ctx := context.Background()
	key, err := svc.Seed(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	other, _ := GenerateKey()
	for name, k := range map[string]string{"unknown": other, "malformed": "garbage", "empty": ""} {
		if _, err := svc.Authenticate(ctx, k); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: got %v, want ErrUnauthorized", name, err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	if _, err := svc.Authenticate(ctx, key); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked: got %v, want ErrUnauthorized", err)
	}
	out := logs.String()
	if !strings.Contains(out, "revoked api key used") || !strings.Contains(out, "api_key_id") {
		t.Errorf("revoked use should log with the key id: %s", out)
	}
	if strings.Contains(out, key) {
		t.Error("the raw key must never be logged")
	}
}

type probe struct {
	Principal Principal
	OK        bool
}

func TestMiddleware(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService(pool)
	ctx := context.Background()
	key, err := svc.Seed(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	var seen probe
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(httpx.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	defer slog.SetDefault(prev)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	r.Group(func(r chi.Router) {
		r.Use(Middleware(svc))
		r.Get("/private", func(w http.ResponseWriter, r *http.Request) {
			seen.Principal, seen.OK = FromContext(r.Context())
			slog.InfoContext(r.Context(), "in handler")
			w.WriteHeader(http.StatusNoContent)
		})
	})

	do := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/private", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	t.Run("valid key", func(t *testing.T) {
		rec := do("Bearer " + key)
		if rec.Code != http.StatusNoContent || !seen.OK || seen.Principal.BusinessID == uuid.Nil {
			t.Fatalf("code=%d principal=%+v", rec.Code, seen)
		}
		if !strings.Contains(logs.String(), `"business_id":"`+seen.Principal.BusinessID.String()) ||
			!strings.Contains(logs.String(), `"api_key_id":"`+seen.Principal.APIKeyID.String()) {
			t.Errorf("handler logs should carry business_id and api_key_id: %s", logs.String())
		}
		if strings.Contains(logs.String(), key) {
			t.Error("the raw key must never be logged")
		}
	})
	t.Run("scheme is case-insensitive", func(t *testing.T) {
		if rec := do("bearer " + key); rec.Code != http.StatusNoContent {
			t.Fatalf("code=%d", rec.Code)
		}
	})

	if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id, business_id, prefix, key_hash, revoked_at)
		SELECT $1, business_id, 'sk_test_dead', $2, now() FROM api_keys LIMIT 1`,
		uuid.New(), HashKey(demoKey)); err != nil {
		t.Fatal(err)
	}
	unknown, _ := GenerateKey()

	// Missing, malformed, unknown and revoked keys must be indistinguishable.
	var bodies []string
	for name, h := range map[string]string{
		"missing":      "",
		"wrong scheme": "Basic " + key,
		"no token":     "Bearer ",
		"bare token":   key,
		"unknown":      "Bearer " + unknown,
		"revoked":      "Bearer " + demoKey,
	} {
		rec := do(h)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d, want 401", name, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: missing WWW-Authenticate", name)
		}
		var e struct {
			Error map[string]string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error["code"] != "unauthorized" || e.Error["request_id"] == "" {
			t.Errorf("%s: bad envelope %q", name, rec.Body.String())
		}
		delete(e.Error, "request_id")
		b, _ := json.Marshal(e)
		bodies = append(bodies, string(b))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("401 bodies differ: %s vs %s", b, bodies[0])
		}
	}
}

func TestMiddlewareReturns500WhenTheDatabaseFails(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService(pool)
	pool.Close()

	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Group(func(r chi.Router) {
		r.Use(Middleware(svc))
		r.Get("/private", func(http.ResponseWriter, *http.Request) {})
	})
	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	req.Header.Set("Authorization", "Bearer "+demoKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a database failure must be 500, not 401: got %d", rec.Code)
	}
}

func TestFromContextOutsideAuthenticatedRoute(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("no principal expected")
	}
}
