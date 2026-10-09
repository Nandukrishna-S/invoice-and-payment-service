package customers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/testapi"
)

type customerJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type pageJSON struct {
	Data    []customerJSON `json:"data"`
	HasMore bool           `json:"has_more"`
}

func setup(t *testing.T) (*testapi.Env, testapi.Tenant, testapi.Tenant) {
	t.Helper()
	env := testapi.New(t, func(r chi.Router, pool *pgxpool.Pool) { RegisterRoutes(r, pool) })
	return env, env.NewTenant(t), env.NewTenant(t)
}

func create(t *testing.T, env *testapi.Env, key, name string) customerJSON {
	t.Helper()
	resp := env.Do(t, key, http.MethodPost, "/customers", map[string]string{"name": name, "email": "a@example.com"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.Code, resp.Body)
	}
	var c customerJSON
	resp.JSON(t, &c)
	return c
}

func TestCreate(t *testing.T) {
	env, a, _ := setup(t)

	resp := env.Do(t, a.Key, http.MethodPost, "/customers",
		map[string]string{"name": "  Acme Corp ", "email": " billing@acme.test "})
	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
	var c customerJSON
	resp.JSON(t, &c)
	id, err := uuid.Parse(c.ID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("id %q must be a UUIDv7", c.ID)
	}
	if c.Name != "Acme Corp" || c.Email != "billing@acme.test" || c.CreatedAt.IsZero() {
		t.Fatalf("unexpected customer: %+v", c)
	}
	if strings.Contains(string(resp.Body), "business_id") {
		t.Fatal("business_id must not be exposed")
	}

	var stored uuid.UUID
	if err := env.Pool.QueryRow(t.Context(), `SELECT business_id FROM customers WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != a.BusinessID {
		t.Fatal("customer must belong to the authenticated business")
	}
}

func TestCreateValidation(t *testing.T) {
	env, a, _ := setup(t)
	tests := []struct {
		name      string
		body      any
		wantCode  string
		wantField string
		status    int
	}{
		{"missing name", map[string]string{"email": "a@b.co"}, "validation_failed", "name", 422},
		{"blank name", map[string]string{"name": "   ", "email": "a@b.co"}, "validation_failed", "name", 422},
		{"name too long", map[string]string{"name": strings.Repeat("é", 201), "email": "a@b.co"}, "validation_failed", "name", 422},
		{"NUL byte in name", `{"name":"a\u0000b","email":"a@b.co"}`, "validation_failed", "name", 422},
		{"newline in name", `{"name":"a\nb","email":"a@b.co"}`, "validation_failed", "name", 422},
		{"NUL byte in email", `{"name":"x","email":"a\u0000@b.co"}`, "validation_failed", "email", 422},
		{"missing email", map[string]string{"name": "x"}, "validation_failed", "email", 422},
		{"email without at", map[string]string{"name": "x", "email": "nope"}, "validation_failed", "email", 422},
		{"email with display name", map[string]string{"name": "x", "email": "Bob <bob@example.com>"}, "validation_failed", "email", 422},
		{"email too long", map[string]string{"name": "x", "email": strings.Repeat("a", 320) + "@b.co"}, "validation_failed", "email", 422},
		{"name wrong type", `{"name":5,"email":"a@b.co"}`, "validation_failed", "name", 422},
		{"unknown field", `{"name":"x","email":"a@b.co","nickname":"y"}`, "unknown_field", "nickname", 400},
		{"business_id is never accepted", fmt.Sprintf(`{"name":"x","email":"a@b.co","business_id":%q}`, uuid.New()), "unknown_field", "business_id", 400},
		{"malformed json", `{"name":`, "invalid_json", "", 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := env.Do(t, a.Key, http.MethodPost, "/customers", tt.body)
			if resp.Code != tt.status || resp.ErrorCode(t) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", resp.Code, resp.Body, tt.status, tt.wantCode)
			}
			if tt.wantField != "" && !strings.Contains(resp.ErrorMessage(t), tt.wantField) {
				t.Fatalf("message %q should name %q", resp.ErrorMessage(t), tt.wantField)
			}
		})
	}

	var n int
	if err := env.Pool.QueryRow(t.Context(), `SELECT count(*) FROM customers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected requests must not insert: n=%d err=%v", n, err)
	}
}

func TestCreateRequiresJSONContentType(t *testing.T) {
	env, a, _ := setup(t)
	resp := env.Do(t, a.Key, http.MethodPost, "/customers", nil)
	if resp.Code != http.StatusUnsupportedMediaType || resp.ErrorCode(t) != "unsupported_media_type" {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
}

func TestAuthIsRequiredOnEveryRoute(t *testing.T) {
	env, _, _ := setup(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/customers"},
		{http.MethodGet, "/customers"},
		{http.MethodGet, "/customers/" + uuid.NewString()},
	} {
		if resp := env.Do(t, "", tt.method, tt.path, nil); resp.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key: got %d", tt.method, tt.path, resp.Code)
		}
	}
}

func TestGetAndTenantIsolation(t *testing.T) {
	env, a, b := setup(t)
	c := create(t, env, a.Key, "Acme")

	resp := env.Do(t, a.Key, http.MethodGet, "/customers/"+c.ID, nil)
	var got customerJSON
	resp.JSON(t, &got)
	if resp.Code != http.StatusOK || got != c {
		t.Fatalf("owner read: %d %s", resp.Code, resp.Body)
	}

	// Another business's customer is indistinguishable from one that doesn't exist.
	resp = env.Do(t, b.Key, http.MethodGet, "/customers/"+c.ID, nil)
	if resp.Code != http.StatusNotFound || resp.ErrorCode(t) != "customer_not_found" {
		t.Errorf("cross-tenant read: got %d %s", resp.Code, resp.Body)
	}
	for name, path := range map[string]string{
		"nonexistent id":     "/customers/" + uuid.NewString(),
		"malformed id":       "/customers/not-a-uuid",
		"non-canonical uuid": "/customers/{" + c.ID + "}",
	} {
		resp := env.Do(t, a.Key, http.MethodGet, path, nil)
		if resp.Code != http.StatusNotFound || resp.ErrorCode(t) != "customer_not_found" {
			t.Errorf("%s: got %d %s", name, resp.Code, resp.Body)
		}
	}
}

func TestSameEmailAcrossAndWithinBusinesses(t *testing.T) {
	env, a, b := setup(t)
	for _, key := range []string{a.Key, b.Key, a.Key} {
		create(t, env, key, "Same Person") // all use a@example.com
	}
	var n int
	err := env.Pool.QueryRow(t.Context(), `SELECT count(*) FROM customers WHERE email = 'a@example.com'`).Scan(&n)
	if err != nil || n != 3 {
		t.Fatalf("emails are not unique per business or globally: got %d rows, err %v", n, err)
	}
}

func list(t *testing.T, env *testapi.Env, key, query string) (pageJSON, int) {
	t.Helper()
	resp := env.Do(t, key, http.MethodGet, "/customers"+query, nil)
	var p pageJSON
	if resp.Code == http.StatusOK {
		resp.JSON(t, &p)
	}
	return p, resp.Code
}

func TestListPaginatesNewestFirst(t *testing.T) {
	env, a, b := setup(t)
	var ids []string // creation order
	for i := 0; i < 5; i++ {
		ids = append(ids, create(t, env, a.Key, fmt.Sprintf("c%d", i)).ID)
	}
	create(t, env, b.Key, "other tenant")

	var seen []string
	query, pages := "?limit=2", 0
	for {
		p, code := list(t, env, a.Key, query)
		if code != http.StatusOK {
			t.Fatalf("list: %d", code)
		}
		pages++
		for _, c := range p.Data {
			seen = append(seen, c.ID)
		}
		if !p.HasMore {
			break
		}
		if len(p.Data) != 2 {
			t.Fatalf("a page with has_more must be full, got %d", len(p.Data))
		}
		query = "?limit=2&starting_after=" + p.Data[len(p.Data)-1].ID
	}

	if pages != 3 || len(seen) != 5 {
		t.Fatalf("pages=%d items=%d, want 3 pages and 5 items", pages, len(seen))
	}
	for i, id := range seen {
		if want := ids[len(ids)-1-i]; id != want {
			t.Fatalf("position %d: got %s, want %s (newest first, no duplicates or gaps)", i, id, want)
		}
	}
}

func TestListExactlyOnePageHasNoMore(t *testing.T) {
	env, a, _ := setup(t)
	create(t, env, a.Key, "x")
	create(t, env, a.Key, "y")
	if p, _ := list(t, env, a.Key, "?limit=2"); len(p.Data) != 2 || p.HasMore {
		t.Fatalf("exactly a full page must report has_more=false: %+v", p)
	}
}

func TestListEmptyIsAnEmptyArray(t *testing.T) {
	env, a, _ := setup(t)
	resp := env.Do(t, a.Key, http.MethodGet, "/customers", nil)
	if resp.Code != 200 || !strings.Contains(string(resp.Body), `"data":[]`) || !strings.Contains(string(resp.Body), `"has_more":false`) {
		t.Fatalf("got %d %s", resp.Code, resp.Body)
	}
}

func TestListIsScopedToTheBusiness(t *testing.T) {
	env, a, b := setup(t)
	create(t, env, a.Key, "mine")
	other := create(t, env, b.Key, "theirs")

	p, _ := list(t, env, a.Key, "")
	for _, c := range p.Data {
		if c.ID == other.ID {
			t.Fatal("another business's customer leaked into the list")
		}
	}
	if len(p.Data) != 1 {
		t.Fatalf("got %d customers, want 1", len(p.Data))
	}
}

func TestListCursorIsAPositionNotALookup(t *testing.T) {
	env, a, b := setup(t)
	mine := []string{create(t, env, a.Key, "m1").ID, create(t, env, a.Key, "m2").ID}
	theirs := create(t, env, b.Key, "theirs").ID // newer than both of mine

	// A well-formed id that doesn't exist, and another tenant's real id, behave
	// identically: no 404, and nothing reveals whether the id exists.
	for name, cursor := range map[string]string{
		"nonexistent":    uuid.Max.String(),
		"foreign tenant": theirs,
	} {
		p, code := list(t, env, a.Key, "?starting_after="+cursor)
		if code != http.StatusOK || len(p.Data) != 2 || p.Data[0].ID != mine[1] {
			t.Errorf("%s cursor: code=%d data=%+v", name, code, p.Data)
		}
	}
	// A cursor older than everything yields an empty page.
	if p, code := list(t, env, a.Key, "?starting_after="+uuid.Nil.String()); code != 200 || len(p.Data) != 0 || p.HasMore {
		t.Errorf("oldest cursor: code=%d %+v", code, p)
	}
}

func TestListQueryValidation(t *testing.T) {
	env, a, _ := setup(t)
	tests := []struct{ query, field string }{
		{"?limit=0", "limit"},
		{"?limit=101", "limit"},
		{"?limit=-1", "limit"},
		{"?limit=abc", "limit"},
		{"?starting_after=nope", "starting_after"},
		{"?starting_after=" + strings.ReplaceAll(uuid.NewString(), "-", ""), "starting_after"},
	}
	for _, tt := range tests {
		resp := env.Do(t, a.Key, http.MethodGet, "/customers"+tt.query, nil)
		if resp.Code != http.StatusUnprocessableEntity || resp.ErrorCode(t) != "validation_failed" ||
			!strings.Contains(resp.ErrorMessage(t), tt.field) {
			t.Errorf("%s: got %d %s", tt.query, resp.Code, resp.Body)
		}
	}
	if _, code := list(t, env, a.Key, "?limit=100"); code != http.StatusOK {
		t.Errorf("limit=100 should be allowed, got %d", code)
	}
}

// A client that disconnects mid-request is not a server failure: no 500, no body.
func TestClientDisconnectIsNotAServerError(t *testing.T) {
	env, a, _ := setup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/customers", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+a.Key)
	rec := httptest.NewRecorder()
	env.Handler.ServeHTTP(rec, req)

	if rec.Code != 499 || rec.Body.Len() != 0 {
		t.Fatalf("got %d %q, want an empty 499", rec.Code, rec.Body.String())
	}
}
