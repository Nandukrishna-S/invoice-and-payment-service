package invoices

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/customers"
	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/testapi"
	"invoice-and-payment-service/internal/testpsp"
)

type lineJSON struct {
	Description     string `json:"description"`
	Quantity        int64  `json:"quantity"`
	UnitAmountCents int64  `json:"unit_amount_cents"`
	AmountCents     int64  `json:"amount_cents"`
}

type invoiceJSON struct {
	ID                    string     `json:"id"`
	InvoiceSequenceNumber *string    `json:"invoice_sequence_number"`
	CustomerID            string     `json:"customer_id"`
	Status                string     `json:"status"`
	Currency              string     `json:"currency"`
	TotalCents            int64      `json:"total_cents"`
	DueDate               string     `json:"due_date"`
	LineItems             []lineJSON `json:"line_items"`
	PaidAt                *time.Time `json:"paid_at"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type pageJSON struct {
	Data    []invoiceJSON `json:"data"`
	HasMore bool          `json:"has_more"`
}

type fixture struct {
	env      *testapi.Env
	a, b     testapi.Tenant
	customer string // belongs to a
	other    string // belongs to b
	now      *atomic.Pointer[time.Time]
	mock     *testpsp.Mock
	svc      *Service
	// queryClient asks the provider what happened, as the reconciler will.
	queryClient *psp.Client
}

// setClock moves the service's clock, e.g. across a financial-year boundary.
func (f fixture) setClock(t time.Time) { f.now.Store(&t) }

// fiscalIST is the production timezone for the financial-year boundary.
var fiscalIST = mustLoadIST()

func mustLoadIST() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		panic(err)
	}
	return loc
}

func setup(t *testing.T) fixture {
	t.Helper()
	// Most tests never pay; the defaults are only used by those that do.
	return setupPay(t, time.Hour, 5*time.Second)
}

// setupPay starts the real mock provider. processingDelay is how long tok_timeout
// charges stay processing; pspTotal is the pay flow's budget for a provider call.
func setupPay(t *testing.T, processingDelay, pspTotal time.Duration) fixture {
	t.Helper()
	mock := testpsp.Start(t, processingDelay)
	env, now, svc := newEnv(t, psp.NewClient(mock.URL, time.Second, pspTotal))
	f := fixture{env: env, now: now, mock: mock, svc: svc, a: env.NewTenant(t), b: env.NewTenant(t),
		queryClient: psp.NewClient(mock.URL, time.Second, 5*time.Second)}
	f.customer = newCustomer(t, env, f.a.Key)
	f.other = newCustomer(t, env, f.b.Key)
	return f
}

// newEnv serves the customer and invoice routes with a controllable clock
// (starting at 2026-10-09 12:00 IST, financial year 2026-27) and the IST boundary.
func newEnv(t *testing.T, provider PSPClient) (*testapi.Env, *atomic.Pointer[time.Time], *Service) {
	t.Helper()
	now := new(atomic.Pointer[time.Time])
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, fiscalIST)
	now.Store(&start)
	var svc *Service
	env := testapi.New(t, func(r chi.Router, pool *pgxpool.Pool) {
		customers.RegisterRoutes(r, pool)
		svc = NewService(pool, fiscalIST, provider)
		svc.now = func() time.Time { return *now.Load() }
		registerRoutes(r, svc)
	})
	return env, now, svc
}

func newCustomer(t *testing.T, env *testapi.Env, key string) string {
	t.Helper()
	resp := env.Do(t, key, http.MethodPost, "/customers", map[string]string{"name": "Acme", "email": "a@acme.test"})
	var c struct {
		ID string `json:"id"`
	}
	resp.JSON(t, &c)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create customer: %d %s", resp.Code, resp.Body)
	}
	return c.ID
}

func body(customerID string, total int64, lines ...map[string]any) map[string]any {
	if len(lines) == 0 {
		lines = []map[string]any{{"description": "Consulting", "quantity": 3, "unit_amount_cents": 1500}}
	}
	return map[string]any{
		"customer_id":          customerID,
		"due_date":             "2026-11-01",
		"line_items":           lines,
		"expected_total_cents": total,
	}
}

func line(desc string, qty, unit any) map[string]any {
	return map[string]any{"description": desc, "quantity": qty, "unit_amount_cents": unit}
}

func create(t *testing.T, f fixture, key string, customerID string) invoiceJSON {
	t.Helper()
	resp := f.env.Do(t, key, http.MethodPost, "/invoices", body(customerID, 4500))
	if resp.Code != http.StatusCreated {
		t.Fatalf("create invoice: %d %s", resp.Code, resp.Body)
	}
	var inv invoiceJSON
	resp.JSON(t, &inv)
	return inv
}

func count(t *testing.T, env *testapi.Env, table string) int {
	t.Helper()
	var n int
	if err := env.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateComputesTotalsOnTheServer(t *testing.T) {
	f := setup(t)
	resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4500+250,
		line("Consulting (hours)", 3, 1500), line("  Setup fee ", 1, 250)))
	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
	var inv invoiceJSON
	resp.JSON(t, &inv)

	id, err := uuid.Parse(inv.ID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("id %q must be a UUIDv7", inv.ID)
	}
	if inv.Status != "draft" || inv.Currency != "USD" || inv.TotalCents != 4750 || inv.DueDate != "2026-11-01" ||
		inv.CustomerID != f.customer || inv.InvoiceSequenceNumber != nil || inv.PaidAt != nil {
		t.Fatalf("unexpected invoice: %+v", inv)
	}
	if len(inv.LineItems) != 2 ||
		inv.LineItems[0] != (lineJSON{"Consulting (hours)", 3, 1500, 4500}) ||
		inv.LineItems[1] != (lineJSON{"Setup fee", 1, 250, 250}) {
		t.Fatalf("lines must come back in order with server-computed amounts: %+v", inv.LineItems)
	}
	if inv.CreatedAt.IsZero() || !inv.UpdatedAt.Equal(inv.CreatedAt) {
		t.Fatalf("timestamps: %+v", inv)
	}
	if strings.Contains(string(resp.Body), "business_id") {
		t.Fatal("business_id must not be exposed")
	}
	for _, key := range []string{`"invoice_sequence_number":null`, `"paid_at":null`} {
		if !strings.Contains(string(resp.Body), key) {
			t.Errorf("response should contain %s", key)
		}
	}

	ctx := context.Background()
	var business uuid.UUID
	var total int64
	err = f.env.Pool.QueryRow(ctx, `SELECT business_id, total_cents FROM invoices WHERE id = $1`, id).Scan(&business, &total)
	if err != nil || business != f.a.BusinessID || total != 4750 {
		t.Fatalf("stored invoice: business=%v total=%d err=%v", business, total, err)
	}

	// The creation transition is written with the invoice.
	var from *string
	var to, reason string
	err = f.env.Pool.QueryRow(ctx,
		`SELECT from_status, to_status, reason FROM invoice_transitions WHERE invoice_id = $1`, id).Scan(&from, &to, &reason)
	if err != nil || from != nil || to != "draft" || reason != "created" {
		t.Fatalf("transition: from=%v to=%q reason=%q err=%v", from, to, reason, err)
	}
	if n := count(t, f.env, "invoice_transitions"); n != 1 {
		t.Fatalf("got %d transitions, want 1", n)
	}
}

func TestCreateRejectsAMismatchedExpectedTotal(t *testing.T) {
	f := setup(t)
	resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4499))
	if resp.Code != 422 || resp.ErrorCode(t) != "total_mismatch" {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
	assertNothingStored(t, f)
}

func assertNothingStored(t *testing.T, f fixture) {
	t.Helper()
	for _, table := range []string{"invoices", "invoice_line_items", "invoice_transitions"} {
		if n := count(t, f.env, table); n != 0 {
			t.Errorf("%s has %d rows after a rejected request", table, n)
		}
	}
}

func TestCreateOverflow(t *testing.T) {
	f := setup(t)
	tests := []struct {
		name  string
		lines []map[string]any
	}{
		{"quantity times unit", []map[string]any{line("a", int64(math.MaxInt64), 2)}},
		{"sum of lines", []map[string]any{line("a", 1, int64(math.MaxInt64)), line("b", 1, 1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 1, tt.lines...))
			if resp.Code != 422 || resp.ErrorCode(t) != "amount_overflow" {
				t.Fatalf("got %d %s", resp.Code, resp.Body)
			}
		})
	}
	assertNothingStored(t, f)
}

func TestCreateLargestRepresentableTotalRoundTripsExactly(t *testing.T) {
	f := setup(t)
	resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices",
		body(f.customer, math.MaxInt64, line("Everything", 1, int64(math.MaxInt64))))
	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
	var inv invoiceJSON
	resp.JSON(t, &inv)
	if inv.TotalCents != math.MaxInt64 || inv.LineItems[0].AmountCents != math.MaxInt64 {
		t.Fatalf("an int64 must survive the JSON round trip exactly: %+v", inv)
	}
}

func TestCreateValidation(t *testing.T) {
	f := setup(t)
	longDesc := strings.Repeat("é", 501)
	many := make([]map[string]any, 101)
	for i := range many {
		many[i] = line("x", 1, 1)
	}
	with := func(key string, value any) map[string]any {
		b := body(f.customer, 4500)
		b[key] = value
		return b
	}
	without := func(key string) map[string]any {
		b := body(f.customer, 4500)
		delete(b, key)
		return b
	}

	tests := []struct {
		name   string
		body   any
		status int
		code   string
		field  string
	}{
		{"missing customer_id", without("customer_id"), 422, "validation_failed", "customer_id"},
		{"malformed customer_id", with("customer_id", "nope"), 422, "validation_failed", "customer_id"},
		{"missing due_date", without("due_date"), 422, "validation_failed", "due_date"},
		{"due_date wrong format", with("due_date", "11/01/2026"), 422, "validation_failed", "due_date"},
		{"due_date impossible", with("due_date", "2026-13-01"), 422, "validation_failed", "due_date"},
		{"due_date with time", with("due_date", "2026-11-01T00:00:00Z"), 422, "validation_failed", "due_date"},
		{"no line items", with("line_items", []any{}), 422, "validation_failed", "line_items"},
		{"missing line items", without("line_items"), 422, "validation_failed", "line_items"},
		{"too many line items", with("line_items", many), 422, "validation_failed", "line_items"},
		{"blank description", body(f.customer, 4500, line("   ", 3, 1500)), 422, "validation_failed", "line_items[0].description"},
		{"description too long", body(f.customer, 4500, line(longDesc, 3, 1500)), 422, "validation_failed", "line_items[0].description"},
		{"NUL in description", body(f.customer, 4500, line("a\x00b", 3, 1500)), 422, "validation_failed", "line_items[0].description"},
		{"zero quantity", body(f.customer, 4500, line("a", 0, 1500)), 422, "validation_failed", "line_items[0].quantity"},
		{"negative quantity", body(f.customer, 4500, line("a", -1, 1500)), 422, "validation_failed", "line_items[0].quantity"},
		{"zero unit amount", body(f.customer, 4500, line("a", 3, 0)), 422, "validation_failed", "line_items[0].unit_amount_cents"},
		{"second line is the bad one", body(f.customer, 4500, line("ok", 1, 1), line("a", 0, 5)), 422, "validation_failed", "line_items[1].quantity"},
		{"missing expected total", without("expected_total_cents"), 422, "validation_failed", "expected_total_cents"},
		{"zero expected total", with("expected_total_cents", 0), 422, "validation_failed", "expected_total_cents"},
		{"negative expected total", with("expected_total_cents", -5), 422, "validation_failed", "expected_total_cents"},
		{"quantity as string", `{"customer_id":"` + f.customer + `","due_date":"2026-11-01","line_items":[{"description":"a","quantity":"3","unit_amount_cents":1}],"expected_total_cents":3}`, 422, "validation_failed", "quantity"},
		{"fractional quantity", `{"customer_id":"` + f.customer + `","due_date":"2026-11-01","line_items":[{"description":"a","quantity":1.5,"unit_amount_cents":2}],"expected_total_cents":3}`, 422, "validation_failed", "quantity"},
		{"fractional expected total", `{"customer_id":"` + f.customer + `","due_date":"2026-11-01","line_items":[{"description":"a","quantity":1,"unit_amount_cents":1}],"expected_total_cents":1.0}`, 422, "validation_failed", "expected_total_cents"},
		{"client cannot set the total", with("total_cents", 1), 400, "unknown_field", "total_cents"},
		{"client cannot set the status", with("status", "paid"), 400, "unknown_field", "status"},
		{"client cannot set business_id", with("business_id", uuid.NewString()), 400, "unknown_field", "business_id"},
		{"unknown line field", body(f.customer, 4500, map[string]any{"description": "a", "quantity": 1, "unit_amount_cents": 1, "amount_cents": 1}), 400, "unknown_field", "amount_cents"},
		{"malformed json", `{"customer_id":`, 400, "invalid_json", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", tt.body)
			if resp.Code != tt.status || resp.ErrorCode(t) != tt.code {
				t.Fatalf("got %d %s, want %d %s", resp.Code, resp.Body, tt.status, tt.code)
			}
			if tt.field != "" && !strings.Contains(resp.ErrorMessage(t), tt.field) {
				t.Fatalf("message %q should name %q", resp.ErrorMessage(t), tt.field)
			}
		})
	}
	assertNothingStored(t, f)
}

func TestCreateAcceptsAPastDueDate(t *testing.T) {
	f := setup(t)
	b := body(f.customer, 4500)
	b["due_date"] = "2001-02-03"
	if resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", b); resp.Code != http.StatusCreated {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
}

func TestCreateForAnotherBusinesssCustomerIsNotFound(t *testing.T) {
	f := setup(t)
	for name, customer := range map[string]string{
		"another business's customer": f.other,
		"nonexistent customer":        uuid.NewString(),
	} {
		resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(customer, 4500))
		if resp.Code != 404 || resp.ErrorCode(t) != "customer_not_found" {
			t.Errorf("%s: got %d %s", name, resp.Code, resp.Body)
		}
	}
	assertNothingStored(t, f)
}

// If any write in the transaction fails, none of it may remain.
func TestCreateIsAtomic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.env.Pool.Exec(ctx, `
		CREATE FUNCTION fail_transition() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'simulated failure'; END $$;
		CREATE TRIGGER fail_transition BEFORE INSERT ON invoice_transitions
		FOR EACH ROW EXECUTE FUNCTION fail_transition();`)
	if err != nil {
		t.Fatal(err)
	}

	resp := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4500))
	if resp.Code != http.StatusInternalServerError || resp.ErrorCode(t) != "internal_error" {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
	if strings.Contains(string(resp.Body), "simulated failure") {
		t.Fatal("the database error leaked to the client")
	}
	assertNothingStored(t, f)
}

func TestAuthIsRequiredOnEveryRoute(t *testing.T) {
	f := setup(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/invoices"},
		{http.MethodGet, "/invoices"},
		{http.MethodGet, "/invoices/" + uuid.NewString()},
	} {
		if resp := f.env.Do(t, "", tt.method, tt.path, nil); resp.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key: got %d", tt.method, tt.path, resp.Code)
		}
	}
}

func TestGetAndTenantIsolation(t *testing.T) {
	f := setup(t)
	inv := create(t, f, f.a.Key, f.customer)

	resp := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+inv.ID, nil)
	var got invoiceJSON
	resp.JSON(t, &got)
	if resp.Code != http.StatusOK || got.ID != inv.ID || got.TotalCents != 4500 || len(got.LineItems) != 1 || got.LineItems[0].AmountCents != 4500 {
		t.Fatalf("owner read: %d %s", resp.Code, resp.Body)
	}

	for name, tt := range map[string]struct{ key, path string }{
		"another business's invoice": {f.b.Key, "/invoices/" + inv.ID},
		"nonexistent id":             {f.a.Key, "/invoices/" + uuid.NewString()},
		"malformed id":               {f.a.Key, "/invoices/not-a-uuid"},
	} {
		resp := f.env.Do(t, tt.key, http.MethodGet, tt.path, nil)
		if resp.Code != 404 || resp.ErrorCode(t) != "invoice_not_found" {
			t.Errorf("%s: got %d %s", name, resp.Code, resp.Body)
		}
	}
}

func list(t *testing.T, f fixture, key, query string) (pageJSON, int) {
	t.Helper()
	resp := f.env.Do(t, key, http.MethodGet, "/invoices"+query, nil)
	var p pageJSON
	if resp.Code == http.StatusOK {
		resp.JSON(t, &p)
	}
	return p, resp.Code
}

func TestListPaginatesNewestFirstWithLineItems(t *testing.T) {
	f := setup(t)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, create(t, f, f.a.Key, f.customer).ID)
	}
	create(t, f, f.b.Key, f.other)

	var seen []string
	query, pages := "?limit=2", 0
	for {
		p, code := list(t, f, f.a.Key, query)
		if code != http.StatusOK {
			t.Fatalf("list: %d", code)
		}
		pages++
		for _, inv := range p.Data {
			seen = append(seen, inv.ID)
			if len(inv.LineItems) != 1 || inv.LineItems[0].AmountCents != 4500 {
				t.Fatalf("each listed invoice needs its line items: %+v", inv)
			}
		}
		if !p.HasMore {
			break
		}
		query = "?limit=2&starting_after=" + p.Data[len(p.Data)-1].ID
	}
	if pages != 3 || len(seen) != 5 {
		t.Fatalf("pages=%d items=%d, want 3 and 5", pages, len(seen))
	}
	for i, id := range seen {
		if want := ids[len(ids)-1-i]; id != want {
			t.Fatalf("position %d: got %s, want %s", i, id, want)
		}
	}
}

func TestListFiltersByStatus(t *testing.T) {
	f := setup(t)
	draft := create(t, f, f.a.Key, f.customer)
	open := create(t, f, f.a.Key, f.customer)
	foreign := create(t, f, f.b.Key, f.other)
	// Finalize arrives in the next checkpoint; set the state directly for the filter.
	for _, id := range []string{open.ID, foreign.ID} {
		if _, err := f.env.Pool.Exec(context.Background(), `UPDATE invoices SET status = 'open' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	check := func(query string, want ...string) {
		t.Helper()
		p, code := list(t, f, f.a.Key, query)
		if code != http.StatusOK || len(p.Data) != len(want) {
			t.Fatalf("%s: code=%d got %d invoices, want %d", query, code, len(p.Data), len(want))
		}
		for i, inv := range p.Data {
			if inv.ID != want[i] {
				t.Fatalf("%s: position %d is %s, want %s", query, i, inv.ID, want[i])
			}
		}
	}
	check("?status=draft", draft.ID)
	check("?status=open", open.ID) // not the other business's open invoice
	check("?status=paid")
	check("", open.ID, draft.ID)
	check("?status=", open.ID, draft.ID) // an empty filter means no filter

	// The filter composes with pagination.
	p, _ := list(t, f, f.a.Key, "?limit=1")
	if !p.HasMore || len(p.Data) != 1 {
		t.Fatalf("expected a first page of one with more to come: %+v", p)
	}
	check("?status=draft&starting_after="+open.ID, draft.ID)
}

func TestListIsEmptyArrayAndScopedToBusiness(t *testing.T) {
	f := setup(t)
	create(t, f, f.b.Key, f.other)
	resp := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices", nil)
	if resp.Code != 200 || !strings.Contains(string(resp.Body), `"data":[]`) || !strings.Contains(string(resp.Body), `"has_more":false`) {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
}

func TestListCursorIsAPositionNotALookup(t *testing.T) {
	f := setup(t)
	mine := []string{create(t, f, f.a.Key, f.customer).ID, create(t, f, f.a.Key, f.customer).ID}
	theirs := create(t, f, f.b.Key, f.other).ID // newer than both of mine

	for name, cursor := range map[string]string{"nonexistent": uuid.Max.String(), "foreign tenant": theirs} {
		p, code := list(t, f, f.a.Key, "?starting_after="+cursor)
		if code != 200 || len(p.Data) != 2 || p.Data[0].ID != mine[1] {
			t.Errorf("%s cursor: code=%d %+v", name, code, p.Data)
		}
	}
}

func TestListQueryValidation(t *testing.T) {
	f := setup(t)
	for _, tt := range []struct{ query, field string }{
		{"?status=bogus", "status"},
		{"?status=PAID", "status"},
		{"?limit=0", "limit"},
		{"?limit=101", "limit"},
		{"?starting_after=nope", "starting_after"},
	} {
		resp := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices"+tt.query, nil)
		if resp.Code != 422 || resp.ErrorCode(t) != "validation_failed" || !strings.Contains(resp.ErrorMessage(t), tt.field) {
			t.Errorf("%s: got %d %s", tt.query, resp.Code, resp.Body)
		}
	}
}

// The schema is the second line of defence: these writes must fail even if the
// application layer is bypassed or buggy.
func TestDatabaseConstraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inv := create(t, f, f.a.Key, f.customer)
	other := create(t, f, f.b.Key, f.other)

	tests := []struct {
		name, sql string
		args      []any
		wantCode  string // SQLSTATE
	}{
		{"line amount must equal quantity x unit",
			`INSERT INTO invoice_line_items VALUES ($1, 9, 'x', 2, 3, 7)`, []any{inv.ID}, "23514"},
		{"line quantity must be positive",
			`INSERT INTO invoice_line_items VALUES ($1, 9, 'x', 0, 3, 0)`, []any{inv.ID}, "23514"},
		{"line position is unique per invoice",
			`INSERT INTO invoice_line_items VALUES ($1, 1, 'x', 1, 1, 1)`, []any{inv.ID}, "23505"},
		{"total must be positive",
			`UPDATE invoices SET total_cents = 0 WHERE id = $1`, []any{inv.ID}, "23514"},
		{"status must be a known state",
			`UPDATE invoices SET status = 'refunded' WHERE id = $1`, []any{inv.ID}, "23514"},
		{"paid requires paid_at",
			`UPDATE invoices SET status = 'paid' WHERE id = $1`, []any{inv.ID}, "23514"},
		{"paid_at requires paid status",
			`UPDATE invoices SET paid_at = now() WHERE id = $1`, []any{inv.ID}, "23514"},
		{"currency is USD only",
			`UPDATE invoices SET currency = 'EUR' WHERE id = $1`, []any{inv.ID}, "23514"},
		{"sequence number is at most 16 characters",
			`UPDATE invoices SET invoice_sequence_number = 'INV-0000042/26-27X' WHERE id = $1`, []any{inv.ID}, "23514"},
		{"sequence number uses a restricted alphabet",
			`UPDATE invoices SET invoice_sequence_number = 'INV 42' WHERE id = $1`, []any{inv.ID}, "23514"},
		{"an invoice cannot point at another business's customer",
			`UPDATE invoices SET customer_id = $2 WHERE id = $1`, []any{inv.ID, f.other}, "23503"},
		{"transition states must be known",
			`INSERT INTO invoice_transitions (id, invoice_id, to_status, reason) VALUES (gen_random_uuid(), $1, 'bogus', 'x')`, []any{other.ID}, "23514"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.env.Pool.Exec(ctx, tt.sql, tt.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.wantCode {
				t.Fatalf("got %v, want SQLSTATE %s", err, tt.wantCode)
			}
		})
	}
}
