package webhooks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/testapi"
)

// ---- secrets ------------------------------------------------------------------

func TestSecretFormatAndDecoding(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b {
		t.Fatal("secrets must be unique")
	}
	if !strings.HasPrefix(a, "whsec_") {
		t.Fatalf("secret %q must start with whsec_", a)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "whsec_"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("the body must be base64 of 32 bytes: len=%d err=%v", len(raw), err)
	}
	key, err := SecretKey(a)
	if err != nil || !bytes.Equal(key, raw) {
		t.Fatalf("SecretKey must return the decoded bytes: %v", err)
	}
}

func TestSecretKeyAcceptsTheStandardLengths(t *testing.T) {
	for _, n := range []int{16, 24, 32, 64} {
		if _, err := SecretKey("whsec_" + base64.StdEncoding.EncodeToString(make([]byte, n))); err != nil {
			t.Errorf("%d-byte key should be accepted: %v", n, err)
		}
	}
}

func TestSecretKeyRejectsMalformedSecrets(t *testing.T) {
	short := "whsec_" + base64.StdEncoding.EncodeToString([]byte("too short"))
	for name, s := range map[string]string{
		"no prefix":    base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"wrong prefix": "sk_test_" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"not base64":   "whsec_!!!not-base64!!!",
		"wrong length": short,
		"empty":        "",
		"prefix only":  "whsec_",
	} {
		if _, err := SecretKey(s); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
}

// ---- URL validation ------------------------------------------------------------

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		ok   bool
	}{
		{"https", "https://example.com/webhooks", true},
		{"http (allowed for the demo receiver)", "http://receiver:9000/hook", true},
		{"with port and query", "https://example.com:8443/a/b?x=1", true},
		{"ip literal", "http://127.0.0.1:9000/", true},
		{"empty", "", false},
		{"no scheme", "example.com/hook", false},
		{"ftp", "ftp://example.com/hook", false},
		{"javascript", "javascript:alert(1)", false},
		{"file", "file:///etc/passwd", false},
		{"scheme only", "https://", false},
		{"no host", "https:///path", false},
		{"credentials", "https://user:pass@example.com/hook", false},
		{"username only", "https://user@example.com/hook", false},
		{"space", "https://example.com/a b", false},
		{"newline", "https://example.com/a\nb", false},
		{"NUL", "https://example.com/a\x00b", false},
		{"too long", "https://example.com/" + strings.Repeat("a", 2040), false},
		{"exactly 2048", "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/")), true},
	}
	for _, tt := range tests {
		err := validateURL(tt.url)
		if (err == nil) != tt.ok {
			t.Errorf("%s: validateURL(%.60q) = %v, want ok=%v", tt.name, tt.url, err, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "url") {
			t.Errorf("%s: error %q should name the field", tt.name, err)
		}
	}
}

// ---- HTTP ---------------------------------------------------------------------

type endpointJSON struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Secret     string     `json:"secret"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

func setup(t *testing.T) (*testapi.Env, testapi.Tenant, testapi.Tenant) {
	t.Helper()
	env := testapi.New(t, func(r chi.Router, pool *pgxpool.Pool) { RegisterRoutes(r, pool) })
	return env, env.NewTenant(t), env.NewTenant(t)
}

func register(t *testing.T, env *testapi.Env, key, url string) endpointJSON {
	t.Helper()
	r := env.Do(t, key, http.MethodPost, "/webhook_endpoints", map[string]string{"url": url})
	if r.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	var e endpointJSON
	r.JSON(t, &e)
	return e
}

func TestRegisterReturnsTheSecretOnce(t *testing.T) {
	env, a, _ := setup(t)
	logs := captureLogs(t)

	r := env.Do(t, a.Key, http.MethodPost, "/webhook_endpoints", map[string]string{"url": "  https://example.com/hook  "})
	var e endpointJSON
	r.JSON(t, &e)
	if r.Code != http.StatusCreated || e.URL != "https://example.com/hook" || e.DisabledAt != nil || e.CreatedAt.IsZero() {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	if id, err := uuid.Parse(e.ID); err != nil || id.Version() != 7 {
		t.Fatalf("id %q must be a UUIDv7", e.ID)
	}
	if _, err := SecretKey(e.Secret); err != nil {
		t.Fatalf("the returned secret must be usable: %v", err)
	}
	var raw map[string]any
	r.JSON(t, &raw)
	for _, k := range []string{"id", "url", "created_at", "disabled_at", "secret"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response lacks %q", k)
		}
	}
	if strings.Contains(string(r.Body), "business_id") {
		t.Fatal("business_id must not be exposed")
	}

	// The secret is stored as is (signing needs it) and equals the one returned.
	var stored string
	if err := env.Pool.QueryRow(context.Background(), `SELECT secret FROM webhook_endpoints WHERE id = $1`, e.ID).Scan(&stored); err != nil || stored != e.Secret {
		t.Fatalf("stored secret mismatch: %v", err)
	}

	// Never again: not in the list, and not in any log line.
	list := env.Do(t, a.Key, http.MethodGet, "/webhook_endpoints", nil)
	if strings.Contains(string(list.Body), "secret") || strings.Contains(string(list.Body), e.Secret) {
		t.Fatalf("the list must not expose secrets: %s", list.Body)
	}
	if strings.Contains(logs.String(), e.Secret) {
		t.Fatal("the secret reached the logs")
	}
}

func TestEverySecretIsDifferentAndURLsMayRepeat(t *testing.T) {
	env, a, _ := setup(t)
	first := register(t, env, a.Key, "https://example.com/hook")
	second := register(t, env, a.Key, "https://example.com/hook")
	if first.ID == second.ID || first.Secret == second.Secret {
		t.Fatal("two registrations must produce distinct endpoints and secrets")
	}
}

func TestListIsScopedNewestFirstAndIncludesDisabledEndpoints(t *testing.T) {
	env, a, b := setup(t)
	e1 := register(t, env, a.Key, "https://a.example/1")
	e2 := register(t, env, a.Key, "https://a.example/2")
	register(t, env, b.Key, "https://b.example/1")
	if _, err := env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, e1.ID); err != nil {
		t.Fatal(err)
	}

	r := env.Do(t, a.Key, http.MethodGet, "/webhook_endpoints", nil)
	var page struct {
		Data []endpointJSON `json:"data"`
	}
	r.JSON(t, &page)
	if r.Code != 200 || len(page.Data) != 2 || page.Data[0].ID != e2.ID || page.Data[1].ID != e1.ID {
		t.Fatalf("got %d %s, want A's two endpoints newest first", r.Code, r.Body)
	}
	if page.Data[1].DisabledAt == nil || page.Data[0].DisabledAt != nil {
		t.Fatal("disabled_at must show which endpoint is disabled")
	}

	empty := env.NewTenant(t)
	if r := env.Do(t, empty.Key, http.MethodGet, "/webhook_endpoints", nil); !strings.Contains(string(r.Body), `"data":[]`) {
		t.Fatalf("a business with none lists an empty array: %s", r.Body)
	}
}

func TestRegisterValidationAndAuth(t *testing.T) {
	env, a, _ := setup(t)
	tests := []struct {
		name   string
		body   any
		status int
		code   string
		field  string
	}{
		{"missing url", map[string]string{}, 422, "validation_failed", "url"},
		{"blank url", map[string]string{"url": "  "}, 422, "validation_failed", "url"},
		{"not a url", map[string]string{"url": "nope"}, 422, "validation_failed", "url"},
		{"ftp", map[string]string{"url": "ftp://example.com"}, 422, "validation_failed", "url"},
		{"credentials", map[string]string{"url": "https://u:p@example.com"}, 422, "validation_failed", "url"},
		{"url wrong type", `{"url":5}`, 422, "validation_failed", "url"},
		{"unknown field", `{"url":"https://example.com","secret":"mine"}`, 400, "unknown_field", "secret"},
		{"business_id is never accepted", `{"url":"https://example.com","business_id":"` + uuid.NewString() + `"}`, 400, "unknown_field", "business_id"},
		{"malformed json", `{"url":`, 400, "invalid_json", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := env.Do(t, a.Key, http.MethodPost, "/webhook_endpoints", tt.body)
			if r.Code != tt.status || r.ErrorCode(t) != tt.code {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}
			if tt.field != "" && !strings.Contains(r.ErrorMessage(t), tt.field) {
				t.Fatalf("message %q should name %q", r.ErrorMessage(t), tt.field)
			}
		})
	}
	var n int
	if err := env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_endpoints`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected requests stored %d endpoints", n)
	}
	if r := env.Do(t, a.Key, http.MethodPost, "/webhook_endpoints", nil); r.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("no body/content type: %d", r.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodGet} {
		if r := env.Do(t, "", m, "/webhook_endpoints", map[string]string{"url": "https://example.com"}); r.Code != http.StatusUnauthorized {
			t.Errorf("%s without a key: %d", m, r.Code)
		}
	}
}

// ---- the outbox ------------------------------------------------------------

type outboxEnv struct {
	env    *testapi.Env
	outbox *Outbox
	a, b   testapi.Tenant
}

func setupOutbox(t *testing.T) outboxEnv {
	t.Helper()
	env, a, b := setup(t)
	return outboxEnv{env: env, outbox: NewOutbox(0), a: a, b: b}
}

func (o outboxEnv) enqueue(t *testing.T, businessID uuid.UUID, eventType string, snapshot func(context.Context) (any, error)) error {
	t.Helper()
	return pgx.BeginFunc(context.Background(), o.env.Pool, func(tx pgx.Tx) error {
		return o.outbox.Enqueue(context.Background(), tx, businessID, eventType, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), snapshot)
	})
}

func fixedObject(obj any) func(context.Context) (any, error) {
	return func(context.Context) (any, error) { return obj, nil }
}

type deliveryRow struct {
	ID, EventID, EndpointID, Type, Status string
	Payload                               []byte
	Attempts                              int
	Due                                   time.Time
}

func (o outboxEnv) deliveries(t *testing.T) []deliveryRow {
	t.Helper()
	rows, err := o.env.Pool.Query(context.Background(),
		`SELECT id::text, event_id, endpoint_id::text, type, status, payload::text, attempt_count, next_attempt_at
		 FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		var payload string
		if err := rows.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Type, &d.Status, &payload, &d.Attempts, &d.Due); err != nil {
			t.Fatal(err)
		}
		d.Payload = []byte(payload)
		out = append(out, d)
	}
	return out
}

func TestEnqueueFansOutOneRowPerActiveEndpointSharingOneEvent(t *testing.T) {
	o := setupOutbox(t)
	e1 := register(t, o.env, o.a.Key, "https://a.example/1")
	e2 := register(t, o.env, o.a.Key, "https://a.example/2")
	e3 := register(t, o.env, o.a.Key, "https://a.example/3")
	disabled := register(t, o.env, o.a.Key, "https://a.example/disabled")
	other := register(t, o.env, o.b.Key, "https://b.example/1")
	if _, err := o.env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, disabled.ID); err != nil {
		t.Fatal(err)
	}

	if err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, fixedObject(map[string]any{"id": "inv_1"})); err != nil {
		t.Fatal(err)
	}
	rows := o.deliveries(t)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want one per active endpoint of business A (3)", len(rows))
	}
	got := map[string]bool{}
	for _, d := range rows {
		got[d.EndpointID] = true
		if d.EventID != rows[0].EventID || !strings.HasPrefix(d.EventID, "evt_") {
			t.Fatalf("rows of one event must share one evt_ id: %q vs %q", d.EventID, rows[0].EventID)
		}
		if d.Type != EventInvoiceCreated || d.Status != "pending" || d.Attempts != 0 {
			t.Fatalf("new rows are pending, untried: %+v", d)
		}
		if time.Since(d.Due) > time.Minute || d.Due.After(time.Now().Add(time.Minute)) {
			t.Fatalf("the first attempt must be due now, got %v", d.Due)
		}
	}
	for _, id := range []string{e1.ID, e2.ID, e3.ID} {
		if !got[id] {
			t.Errorf("active endpoint %s got no row", id)
		}
	}
	if got[disabled.ID] || got[other.ID] {
		t.Fatal("a disabled endpoint and another business's endpoint must get nothing")
	}
}

func TestEnqueuePayloadIsTheDeliveredEventBody(t *testing.T) {
	o := setupOutbox(t)
	register(t, o.env, o.a.Key, "https://a.example/1")
	if err := o.enqueue(t, o.a.BusinessID, EventInvoicePaid, fixedObject(map[string]any{"id": "inv_1", "status": "paid"})); err != nil {
		t.Fatal(err)
	}
	d := o.deliveries(t)[0]
	var body struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
		Data      struct {
			Object map[string]any `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(d.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != d.EventID || body.Type != "invoice.paid" || !body.CreatedAt.Equal(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)) ||
		body.Data.Object["status"] != "paid" {
		t.Fatalf("payload = %s", d.Payload)
	}
}

func TestEnqueueWithNoEndpointsWritesNothingAndSkipsTheSnapshot(t *testing.T) {
	o := setupOutbox(t)
	register(t, o.env, o.b.Key, "https://b.example/1") // someone else's
	called := false
	err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, func(context.Context) (any, error) {
		called = true
		return nil, nil
	})
	if err != nil || called || len(o.deliveries(t)) != 0 {
		t.Fatalf("err=%v snapshotCalled=%v rows=%d; nobody is listening, so nothing may be built or written", err, called, len(o.deliveries(t)))
	}
}

func TestEndpointsRegisteredLaterDoNotReceivePastEvents(t *testing.T) {
	o := setupOutbox(t)
	early := register(t, o.env, o.a.Key, "https://a.example/early")
	if err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, fixedObject(map[string]any{})); err != nil {
		t.Fatal(err)
	}
	late := register(t, o.env, o.a.Key, "https://a.example/late")
	if err := o.enqueue(t, o.a.BusinessID, EventInvoicePaid, fixedObject(map[string]any{})); err != nil {
		t.Fatal(err)
	}
	perEndpoint := map[string][]string{}
	for _, d := range o.deliveries(t) {
		perEndpoint[d.EndpointID] = append(perEndpoint[d.EndpointID], d.Type)
	}
	if len(perEndpoint[early.ID]) != 2 || len(perEndpoint[late.ID]) != 1 || perEndpoint[late.ID][0] != EventInvoicePaid {
		t.Fatalf("early=%v late=%v: the late endpoint must see only events after it registered", perEndpoint[early.ID], perEndpoint[late.ID])
	}
}

func TestEachEventGetsItsOwnID(t *testing.T) {
	o := setupOutbox(t)
	register(t, o.env, o.a.Key, "https://a.example/1")
	for i := 0; i < 5; i++ {
		if err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, fixedObject(map[string]any{})); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for _, d := range o.deliveries(t) {
		ids[d.EventID] = true
	}
	if len(ids) != 5 {
		t.Fatalf("%d distinct event ids for 5 events", len(ids))
	}
}

// The outbox joins the caller's transaction: a rollback discards the event, a commit keeps it.
func TestEnqueueFollowsTheCallersTransaction(t *testing.T) {
	o := setupOutbox(t)
	register(t, o.env, o.a.Key, "https://a.example/1")
	boom := errors.New("later step failed")

	err := pgx.BeginFunc(context.Background(), o.env.Pool, func(tx pgx.Tx) error {
		if err := o.outbox.Enqueue(context.Background(), tx, o.a.BusinessID, EventInvoiceCreated, time.Now(), fixedObject(map[string]any{})); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || len(o.deliveries(t)) != 0 {
		t.Fatalf("a rolled-back transaction must leave no event (err=%v rows=%d)", err, len(o.deliveries(t)))
	}

	if err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, fixedObject(map[string]any{})); err != nil || len(o.deliveries(t)) != 1 {
		t.Fatalf("a committed transaction must keep it (err=%v rows=%d)", err, len(o.deliveries(t)))
	}
}

func TestSnapshotFailureAbortsTheTransaction(t *testing.T) {
	o := setupOutbox(t)
	register(t, o.env, o.a.Key, "https://a.example/1")
	err := o.enqueue(t, o.a.BusinessID, EventInvoiceCreated, func(context.Context) (any, error) { return nil, errors.New("cannot snapshot") })
	if err == nil || len(o.deliveries(t)) != 0 {
		t.Fatalf("an event that cannot be built must fail the caller, not vanish (err=%v)", err)
	}
}

func TestConcurrentEventsNeverCollide(t *testing.T) {
	o := setupOutbox(t)
	for i := 0; i < 3; i++ {
		register(t, o.env, o.a.Key, "https://a.example/"+string(rune('a'+i)))
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.enqueue(t, o.a.BusinessID, EventInvoicePaid, fixedObject(map[string]any{})); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rows := o.deliveries(t); len(rows) != 60 {
		t.Fatalf("got %d rows, want 20 events x 3 endpoints", len(rows))
	}
}

func TestDeliveriesTableConstraints(t *testing.T) {
	o := setupOutbox(t)
	e := register(t, o.env, o.a.Key, "https://a.example/1")
	ctx := context.Background()
	ins := func(extra string, args ...any) error {
		_, err := o.env.Pool.Exec(ctx, `INSERT INTO webhook_deliveries (id, event_id, endpoint_id, type, payload`+extra, args...)
		return err
	}
	if err := ins(`) VALUES ($1, 'evt_1', $2, 'invoice.paid', '{}')`, uuid.New(), e.ID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"one row per (event, endpoint)", ins(`) VALUES ($1, 'evt_1', $2, 'invoice.paid', '{}')`, uuid.New(), e.ID), "23505"},
		{"only the three event types", ins(`) VALUES ($1, 'evt_2', $2, 'invoice.voided', '{}')`, uuid.New(), e.ID), "23514"},
		{"status must be known", ins(`, status) VALUES ($1, 'evt_3', $2, 'invoice.paid', '{}', 'sent')`, uuid.New(), e.ID), "23514"},
		{"attempt count cannot be negative", ins(`, attempt_count) VALUES ($1, 'evt_4', $2, 'invoice.paid', '{}', -1)`, uuid.New(), e.ID), "23514"},
		{"an endpoint must exist", ins(`) VALUES ($1, 'evt_5', $2, 'invoice.paid', '{}')`, uuid.New(), uuid.New()), "23503"},
		{"payload is required", ins(`) VALUES ($1, 'evt_6', $2, 'invoice.paid', NULL)`, uuid.New(), e.ID), "23502"},
	}
	for _, tt := range tests {
		var pgErr *pgconn.PgError
		if !errors.As(tt.err, &pgErr) || pgErr.Code != tt.code {
			t.Errorf("%s: got %v, want SQLSTATE %s", tt.name, tt.err, tt.code)
		}
	}
	var def string
	if err := o.env.Pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'webhook_deliveries_due'`).Scan(&def); err != nil ||
		!strings.Contains(def, "next_attempt_at") || !strings.Contains(def, "status = 'pending'") {
		t.Fatalf("the due-rows index must cover only pending rows: %q %v", def, err)
	}
	if err := o.env.Pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'webhook_endpoints_active_business'`).Scan(&def); err != nil ||
		!strings.Contains(def, "disabled_at IS NULL") {
		t.Fatalf("the active-endpoint index must cover only active endpoints: %q %v", def, err)
	}
}

func captureLogs(t testing.TB) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
