package mockpsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/httpx"
	"invoice-and-payment-service/internal/testutil"
	"invoice-and-payment-service/migrations/psp"
)

type env struct {
	svc    *Service
	srv    *httptest.Server
	pool   *pgxpool.Pool
	cancel context.CancelFunc
}

// newEnv runs the mock PSP against a private schema. delay is how long
// tok_timeout charges stay processing.
func newEnv(t *testing.T, delay time.Duration) *env {
	t.Helper()
	url := testutil.NewSchema(t)
	if err := db.Migrate(url, psp.FS, "schema_migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.NewPool(context.Background(), url, 20)
	if err != nil {
		t.Fatal(err)
	}
	bg, cancel := context.WithCancel(context.Background())
	svc := NewService(pool, bg, delay, 0)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)
	RegisterRoutes(r, svc)
	srv := httptest.NewServer(r)

	e := &env{svc: svc, srv: srv, pool: pool, cancel: cancel}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		svc.Close()
		pool.Close()
	})
	return e
}

type chargeJSON struct {
	IdempotencyKey string     `json:"idempotency_key"`
	PSPRef         string     `json:"psp_ref"`
	AmountCents    int64      `json:"amount_cents"`
	Status         string     `json:"status"`
	Code           *string    `json:"code"`
	CompletedAt    *time.Time `json:"completed_at"`
}

type reply struct {
	code int
	body []byte
	err  error
}

func (r reply) charge(t *testing.T) chargeJSON {
	t.Helper()
	if r.err != nil {
		t.Fatalf("request failed: %v", r.err)
	}
	var c chargeJSON
	if err := json.Unmarshal(r.body, &c); err != nil {
		t.Fatalf("not a charge (%d): %s", r.code, r.body)
	}
	return c
}

func (r reply) errorCode(t *testing.T) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("not an error envelope (%d): %s", r.code, r.body)
	}
	return e.Error.Code
}

func (e *env) do(client *http.Client, method, path, key, contentType string, body any) reply {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, reader)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return reply{code: resp.StatusCode, body: raw}
}

func (e *env) charge(key, token string, amount int64) reply {
	return e.do(http.DefaultClient, http.MethodPost, "/charges", key, "application/json",
		map[string]any{"token": token, "amount_cents": amount})
}

func (e *env) get(key string) reply {
	return e.do(http.DefaultClient, http.MethodGet, "/charges/"+key, "", "", nil)
}

func (e *env) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM charges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTokenBehaviours(t *testing.T) {
	e := newEnv(t, time.Hour)
	tests := []struct {
		token   string
		status  string
		failure string
	}{
		{"tok_success", "succeeded", ""},
		{"tok_insufficient_funds", "failed", "insufficient_funds"},
		{"tok_card_declined", "failed", "card_declined"},
		{"tok_never_heard_of_it", "failed", "card_declined"},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			r := e.charge("key-"+tt.token, tt.token, 4500)
			c := r.charge(t)
			if r.code != http.StatusOK || c.Status != tt.status || c.AmountCents != 4500 ||
				!isUUID(c.PSPRef) || c.CompletedAt == nil {
				t.Fatalf("got %d %s", r.code, r.body)
			}
			if tt.failure == "" && c.Code != nil || tt.failure != "" && (c.Code == nil || *c.Code != tt.failure) {
				t.Fatalf("code = %v, want %q", c.Code, tt.failure)
			}
		})
	}
}

func TestSameKeyReturnsTheOriginalChargeAndNeverChargesTwice(t *testing.T) {
	e := newEnv(t, time.Hour)
	first := e.charge("k1", "tok_success", 1000).charge(t)
	for i := 0; i < 3; i++ {
		again := e.charge("k1", "tok_success", 1000)
		if got := again.charge(t); again.code != 200 || got.PSPRef != first.PSPRef {
			t.Fatalf("replay %d returned a different charge: %s", i, again.body)
		}
	}
	if n := e.rows(t); n != 1 {
		t.Fatalf("%d charges stored, want 1", n)
	}

	// A declined charge replays as declined; it is not retried.
	declined := e.charge("k2", "tok_card_declined", 500).charge(t)
	if again := e.charge("k2", "tok_card_declined", 500).charge(t); again.PSPRef != declined.PSPRef || again.Status != "failed" {
		t.Fatalf("a declined charge must replay as declined: %+v", again)
	}
}

func TestSameKeyWithDifferentParametersConflicts(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.charge("k1", "tok_success", 1000).charge(t)

	for name, r := range map[string]reply{
		"different amount": e.charge("k1", "tok_success", 1001),
		"different token":  e.charge("k1", "tok_card_declined", 1000),
	} {
		if r.code != http.StatusConflict || r.errorCode(t) != "idempotency_conflict" {
			t.Errorf("%s: got %d %s", name, r.code, r.body)
		}
	}
	if n := e.rows(t); n != 1 {
		t.Fatalf("%d charges stored, want 1", n)
	}
}

func TestGet(t *testing.T) {
	e := newEnv(t, time.Hour)
	made := e.charge("k1", "tok_success", 1000).charge(t)

	r := e.get("k1")
	if got := r.charge(t); r.code != 200 || got.PSPRef != made.PSPRef || got.Status != "succeeded" {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	if r := e.get("never-used"); r.code != http.StatusNotFound || r.errorCode(t) != "charge_not_found" {
		t.Fatalf("got %d %s", r.code, r.body)
	}
}

func TestRequestValidation(t *testing.T) {
	e := newEnv(t, time.Hour)
	tests := []struct {
		name        string
		key         string
		contentType string
		body        any
		status      int
		code        string
	}{
		{"missing idempotency key", "", "application/json", map[string]any{"token": "tok_success", "amount_cents": 1}, 400, "idempotency_key_required"},
		{"key too long", strings.Repeat("k", 256), "application/json", map[string]any{"token": "tok_success", "amount_cents": 1}, 422, "validation_failed"},
		{"missing token", "k", "application/json", map[string]any{"amount_cents": 1}, 422, "validation_failed"},
		{"zero amount", "k", "application/json", map[string]any{"token": "tok_success", "amount_cents": 0}, 422, "validation_failed"},
		{"negative amount", "k", "application/json", map[string]any{"token": "tok_success", "amount_cents": -5}, 422, "validation_failed"},
		{"fractional amount", "k", "application/json", `{"token":"tok_success","amount_cents":1.5}`, 422, "validation_failed"},
		{"unknown field", "k", "application/json", `{"token":"tok_success","amount_cents":1,"currency":"USD"}`, 400, "unknown_field"},
		{"wrong content type", "k", "text/plain", `{"token":"tok_success","amount_cents":1}`, 415, "unsupported_media_type"},
		{"malformed json", "k", "application/json", `{"token":`, 400, "invalid_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do(http.DefaultClient, http.MethodPost, "/charges", tt.key, tt.contentType, tt.body)
			if r.code != tt.status || r.errorCode(t) != tt.code {
				t.Fatalf("got %d %s", r.code, r.body)
			}
		})
	}
	if n := e.rows(t); n != 0 {
		t.Fatalf("rejected requests stored %d charges", n)
	}
}

func waitForStatus(t *testing.T, e *env, key, want string) chargeJSON {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := e.get(key).charge(t)
		if c.Status == want {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("charge %s is %q, want %q", key, c.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSlowChargeHoldsTheResponseThenSucceeds(t *testing.T) {
	e := newEnv(t, 400*time.Millisecond)
	start := time.Now()
	r := e.charge("slow", "tok_timeout", 2500)
	if c := r.charge(t); r.code != 200 || c.Status != "succeeded" || c.CompletedAt == nil {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Fatalf("answered after %v, it must hold the response for the processing delay", elapsed)
	}
}

// The caller (our API, with a 5s timeout) gives up long before the charge
// settles. The charge must still settle, and be visible as processing meanwhile.
func TestSlowChargeSettlesAfterTheCallerDisconnects(t *testing.T) {
	e := newEnv(t, 500*time.Millisecond)

	impatient := &http.Client{Timeout: 100 * time.Millisecond}
	r := e.do(impatient, http.MethodPost, "/charges", "slow", "application/json",
		map[string]any{"token": "tok_timeout", "amount_cents": 2500})
	if r.err == nil {
		t.Fatalf("expected the client to time out, got %d %s", r.code, r.body)
	}

	// Unknown, not failed: the PSP is mid-way through it.
	if c := e.get("slow").charge(t); c.Status != "processing" || c.CompletedAt != nil {
		t.Fatalf("while processing: %+v", c)
	}
	// A retry of the same request answers at once with the current state, and creates nothing new.
	start := time.Now()
	if c := e.charge("slow", "tok_timeout", 2500).charge(t); c.Status != "processing" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("replay while processing: %+v after %v", c, time.Since(start))
	}

	done := waitForStatus(t, e, "slow", "succeeded")
	if done.CompletedAt == nil || e.rows(t) != 1 {
		t.Fatalf("settled charge: %+v, rows=%d", done, e.rows(t))
	}
}

// A charge left processing (e.g. the PSP restarted mid-wait) settles when read once it is old enough.
func TestOldProcessingChargesSettleLazily(t *testing.T) {
	e := newEnv(t, time.Hour)
	ctx := context.Background()
	for key, age := range map[string]string{"old": "2 hours", "young": "1 minute"} {
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, created_at)
			 VALUES ($1, gen_random_uuid()::text, 'tok_timeout', 100, 'processing', now() - $2::interval)`, key, age); err != nil {
			t.Fatal(err)
		}
	}

	if c := e.get("old").charge(t); c.Status != "succeeded" || c.CompletedAt == nil {
		t.Fatalf("old charge on GET: %+v", c)
	}
	if c := e.get("young").charge(t); c.Status != "processing" {
		t.Fatalf("a charge younger than the delay must stay processing: %+v", c)
	}

	// The same applies to a replayed POST.
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, created_at)
		 VALUES ('old2', gen_random_uuid()::text, 'tok_timeout', 100, 'processing', now() - interval '2 hours')`); err != nil {
		t.Fatal(err)
	}
	if c := e.charge("old2", "tok_timeout", 100).charge(t); c.Status != "succeeded" {
		t.Fatalf("old charge on replay: %+v", c)
	}
}

func TestNetworkErrorProcessesTheChargeThenHangsUp(t *testing.T) {
	e := newEnv(t, time.Hour)

	r := e.charge("net", "tok_network_error", 700)
	if r.err == nil {
		t.Fatalf("the connection should be dropped without an answer, got %d %s", r.code, r.body)
	}

	// The charge went through: that is the dangerous part. The caller can only learn it by asking.
	got := e.get("net").charge(t)
	if got.Status != "succeeded" || got.PSPRef == "" {
		t.Fatalf("the dropped call must still have charged: %+v", got)
	}
	// A retry gets the stored answer instead of a second charge.
	again := e.charge("net", "tok_network_error", 700)
	if c := again.charge(t); again.code != 200 || c.PSPRef != got.PSPRef {
		t.Fatalf("replay: %d %s", again.code, again.body)
	}
	if n := e.rows(t); n != 1 {
		t.Fatalf("%d charges, want 1", n)
	}
}

func TestConcurrentIdenticalRequestsMakeOneCharge(t *testing.T) {
	e := newEnv(t, 300*time.Millisecond)
	for _, token := range []string{"tok_success", "tok_timeout"} {
		const n = 20
		var wg sync.WaitGroup
		start := make(chan struct{})
		refs := make([]string, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := e.charge("race-"+token, token, 900)
				if r.err == nil && r.code == 200 {
					refs[i] = r.charge(t).PSPRef
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, ref := range refs {
			if ref != refs[0] || ref == "" {
				t.Fatalf("%s: callers saw different charges: %v", token, refs)
			}
		}
	}
	if n := e.rows(t); n != 2 {
		t.Fatalf("%d charges stored, want 2 (one per key)", n)
	}
	waitForStatus(t, e, "race-tok_timeout", "succeeded")
}

// Shutdown must not hang on a slow charge, and the charge stays processing for lazy settling.
func TestShutdownAbandonsSlowChargesWithoutHanging(t *testing.T) {
	e := newEnv(t, time.Hour)
	errc := make(chan error, 1)
	go func() {
		_, err := e.svc.Charge(context.Background(), "slow", "tok_timeout", 100)
		errc <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for e.rows(t) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	e.cancel()
	closed := make(chan struct{})
	go func() { e.svc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on a slow charge")
	}
	<-errc
	if c := e.get("slow").charge(t); c.Status != "processing" {
		t.Fatalf("an abandoned charge stays processing: %+v", c)
	}
}

func TestDatabaseConstraints(t *testing.T) {
	e := newEnv(t, time.Hour)
	ctx := context.Background()
	tests := []struct {
		name string
		sql  string
		code string
	}{
		{"amount must be positive", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, completed_at) VALUES ('a', 'ch_a', 't', 0, 'succeeded', now())`, "23514"},
		{"status must be known", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, completed_at) VALUES ('b', 'ch_b', 't', 1, 'refunded', now())`, "23514"},
		{"failed needs a failure code", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, completed_at) VALUES ('c', 'ch_c', 't', 1, 'failed', now())`, "23514"},
		{"a failure code needs failed", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, failure_code, completed_at) VALUES ('d', 'ch_d', 't', 1, 'succeeded', 'card_declined', now())`, "23514"},
		{"processing has no completion time", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, completed_at) VALUES ('e', 'ch_e', 't', 1, 'processing', now())`, "23514"},
		{"settled has a completion time", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status) VALUES ('f', 'ch_f', 't', 1, 'succeeded')`, "23514"},
		{"failure code must be known", `INSERT INTO charges (idempotency_key, psp_ref_id, token, amount_cents, status, failure_code, completed_at) VALUES ('g', 'ch_g', 't', 1, 'failed', 'weird', now())`, "23514"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.pool.Exec(ctx, tt.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("got %v, want SQLSTATE %s", err, tt.code)
			}
		})
	}
}

func TestUnmatchedRoutesUseTheEnvelope(t *testing.T) {
	e := newEnv(t, time.Hour)
	if r := e.do(http.DefaultClient, http.MethodGet, "/nope", "", "", nil); r.code != 404 || r.errorCode(t) != "route_not_found" {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	if r := e.do(http.DefaultClient, http.MethodDelete, "/charges/x", "", "", nil); r.code != 405 || r.errorCode(t) != "method_not_allowed" {
		t.Fatalf("got %d %s", r.code, r.body)
	}
}

func isUUID(s string) bool { _, err := uuid.Parse(s); return err == nil && len(s) == 36 }

// The wire format is the provider spec's: psp_ref (a bare UUID) and code, not our own field names.
func TestResponseUsesTheProviderSpecFieldNames(t *testing.T) {
	e := newEnv(t, time.Hour)
	for _, token := range []string{"tok_success", "tok_card_declined"} {
		var raw map[string]any
		r := e.charge("shape-"+token, token, 100)
		if err := json.Unmarshal(r.body, &raw); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"psp_ref", "code", "status"} {
			if _, ok := raw[field]; !ok {
				t.Errorf("%s: response lacks %q: %s", token, field, r.body)
			}
		}
		for _, banned := range []string{"psp_ref_id", "failure_code"} {
			if _, ok := raw[banned]; ok {
				t.Errorf("%s: response must not use our own field name %q: %s", token, banned, r.body)
			}
		}
		if ref, _ := raw["psp_ref"].(string); !isUUID(ref) {
			t.Errorf("%s: psp_ref %q must be a bare UUID", token, ref)
		}
	}
}

// success and decline answer after the fast delay; the delay yields to a caller that gives up.
func TestFastTokensAnswerAfterTheFastDelay(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.svc.fastDelay = 150 * time.Millisecond

	for _, token := range []string{"tok_success", "tok_insufficient_funds", "tok_card_declined"} {
		start := time.Now()
		r := e.charge("fast-"+token, token, 100)
		if r.code != 200 {
			t.Fatalf("%s: %d %s", token, r.code, r.body)
		}
		if elapsed := time.Since(start); elapsed < 120*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Errorf("%s answered after %v, want about 150ms", token, elapsed)
		}
	}

	// A replay returns the stored answer at once; only the first call pays the delay.
	start := time.Now()
	e.charge("fast-tok_success", "tok_success", 100).charge(t)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("a replay took %v", elapsed)
	}

	// tok_network_error drops the connection straight away. A fresh connection is
	// needed: Go's transport transparently retries a request carrying an
	// Idempotency-Key when a reused connection dies, which would hide the drop.
	fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	start = time.Now()
	r := e.do(fresh, http.MethodPost, "/charges", "fast-net", "application/json",
		map[string]any{"token": "tok_network_error", "amount_cents": 100})
	if r.err == nil || time.Since(start) > 100*time.Millisecond {
		t.Errorf("network error: err=%v after %v", r.err, time.Since(start))
	}

	// A caller that gives up during the delay is released promptly; the charge stays.
	impatient := &http.Client{Timeout: 30 * time.Millisecond}
	start = time.Now()
	r = e.do(impatient, http.MethodPost, "/charges", "fast-gone", "application/json",
		map[string]any{"token": "tok_success", "amount_cents": 100})
	if r.err == nil || time.Since(start) > 120*time.Millisecond {
		t.Errorf("impatient caller: err=%v after %v", r.err, time.Since(start))
	}
	if c := e.get("fast-gone").charge(t); c.Status != "succeeded" {
		t.Errorf("a charge whose caller left is still made: %+v", c)
	}

	// The server side must stop waiting too, not sleep out the delay for nobody.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err := e.svc.Charge(ctx, "fast-svc", "tok_success", 100)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 120*time.Millisecond {
		t.Errorf("Charge with a cancelled context: err=%v after %v, want a prompt DeadlineExceeded", err, time.Since(start))
	}
}
