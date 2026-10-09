package invoices

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/reconciler"
	"invoice-and-payment-service/internal/testapi"
)

type attemptJSON struct {
	ID          string     `json:"id"`
	InvoiceID   string     `json:"invoice_id"`
	AmountCents int64      `json:"amount_cents"`
	Status      string     `json:"status"`
	FailureCode *string    `json:"failure_code"`
	PSPRefID    *string    `json:"psp_ref_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
}

var keySeq atomic.Int64

func (a attemptJSON) Resolved() bool { return a.Status != "pending" }

func uniqueKey() string { return fmt.Sprintf("key-%d-%s", keySeq.Add(1), uuid.NewString()[:8]) }

// openInvoice returns a finalized (payable) invoice of 4500 cents.
func openInvoice(t *testing.T, f fixture) invoiceJSON {
	t.Helper()
	inv := create(t, f, f.a.Key, f.customer)
	if r := transition(t, f, f.a.Key, inv.ID, "finalize"); r.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", r.Code, r.Body)
	}
	return inv
}

func payWith(t testing.TB, f fixture, key, invoiceID, idemKey, token string, amount int64) testapi.Response {
	t.Helper()
	req := f.env.Handler
	body := fmt.Sprintf(`{"card_token":%q,"amount_cents":%d}`, token, amount)
	r := httptest.NewRequest(http.MethodPost, "/invoices/"+invoiceID+"/pay", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	req.ServeHTTP(rec, r)
	return testapi.Response{Code: rec.Code, Body: rec.Body.Bytes()}
}

func pay(t testing.TB, f fixture, invoiceID, token string) testapi.Response {
	t.Helper()
	return payWith(t, f, f.a.Key, invoiceID, uniqueKey(), token, 4500)
}

func attemptOf(t testing.TB, r testapi.Response) attemptJSON {
	t.Helper()
	var a attemptJSON
	r.JSON(t, &a)
	return a
}

func getInvoice(t testing.TB, f fixture, id string) invoiceJSON {
	t.Helper()
	var inv invoiceJSON
	f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+id, nil).JSON(t, &inv)
	return inv
}

func countWhere(t testing.TB, f fixture, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.env.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- the brief's table: what every token does through the API -----------------

func TestPayTokensMatchTheSpec(t *testing.T) {
	f := setupPay(t, 700*time.Millisecond, 250*time.Millisecond)

	t.Run("tok_success pays the invoice", func(t *testing.T) {
		inv := openInvoice(t, f)
		r := pay(t, f, inv.ID, "tok_success")
		a := attemptOf(t, r)
		if r.Code != 200 || a.Status != "succeeded" || a.FailureCode != nil || a.PSPRefID == nil || a.ResolvedAt == nil || a.AmountCents != 4500 || a.InvoiceID != inv.ID {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
		if _, err := uuid.Parse(*a.PSPRefID); err != nil {
			t.Fatalf("psp_ref_id %q should be the provider's reference (a UUID)", *a.PSPRefID)
		}
		got := getInvoice(t, f, inv.ID)
		if got.Status != "paid" || got.PaidAt == nil {
			t.Fatalf("invoice after success: %+v", got)
		}
		rows := transitionRows(t, f, inv.ID)
		if len(rows) != 3 || rows[2] != "open>paid:payment_succeeded" {
			t.Fatalf("transitions = %v", rows)
		}
		if n := countWhere(t, f, "invoice_transitions", "invoice_id = $1 AND attempt_id = $2", inv.ID, a.ID); n != 1 {
			t.Fatalf("the paid transition should reference the attempt, found %d", n)
		}
	})

	for token, code := range map[string]string{"tok_insufficient_funds": "insufficient_funds", "tok_card_declined": "card_declined"} {
		t.Run(token+" fails and leaves the invoice alone", func(t *testing.T) {
			inv := openInvoice(t, f)
			r := pay(t, f, inv.ID, token)
			a := attemptOf(t, r)
			if r.Code != 200 || a.Status != "failed" || a.FailureCode == nil || *a.FailureCode != code || a.PSPRefID != nil || a.ResolvedAt == nil {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}
			got := getInvoice(t, f, inv.ID)
			if got.Status != "open" || got.PaidAt != nil {
				t.Fatalf("a decline must not change the invoice: %+v", got)
			}
			if rows := transitionRows(t, f, inv.ID); len(rows) != 2 {
				t.Fatalf("a decline writes no transition: %v", rows)
			}
		})
	}

	t.Run("tok_timeout is accepted and stays pending", func(t *testing.T) {
		inv := openInvoice(t, f)
		start := time.Now()
		r := pay(t, f, inv.ID, "tok_timeout")
		elapsed := time.Since(start)
		a := attemptOf(t, r)
		if r.Code != http.StatusAccepted || a.Status != "pending" || a.ResolvedAt != nil || a.PSPRefID != nil || a.FailureCode != nil {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
		if elapsed < 200*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Fatalf("pay returned after %v, want about the 250ms provider budget", elapsed)
		}
		if got := getInvoice(t, f, inv.ID); got.Status != "open" {
			t.Fatalf("invoice must stay open while unknown: %+v", got)
		}
		// Unknown is not failed: the payment is pending and nothing says otherwise.
		if n := countWhere(t, f, "payments", "status = 'pending'"); n < 1 {
			t.Fatal("the payment must still be pending")
		}
	})

	t.Run("tok_network_error is accepted and stays pending", func(t *testing.T) {
		inv := openInvoice(t, f)
		// Warm the pooled connection first: a dropped call on a reused connection must
		// not be silently resent by the transport.
		pay(t, f, openInvoice(t, f).ID, "tok_success")

		r := pay(t, f, inv.ID, "tok_network_error")
		a := attemptOf(t, r)
		if r.Code != http.StatusAccepted || a.Status != "pending" {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
		if got := getInvoice(t, f, inv.ID); got.Status != "open" {
			t.Fatalf("invoice must stay open: %+v", got)
		}
	})
}

// reconcile runs the real reconciler until the attempt's payment has been settled by
// the provider's answer (the provider may still be working on it for a while).
func reconcile(t testing.TB, f fixture, att attemptJSON) {
	t.Helper()
	rec := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := rec.RunOnce(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+att.ID, nil)); got.Status != "pending" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the reconciler did not settle the attempt in time")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (f fixture) paymentRef(t testing.TB, attemptID string) uuid.UUID {
	t.Helper()
	var ref uuid.UUID
	if err := f.env.Pool.QueryRow(context.Background(),
		`SELECT payment_ref_id FROM invoice_payment_attempts WHERE id = $1`, attemptID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestUnknownOutcomesAreSettledByAskingTheProvider(t *testing.T) {
	for _, token := range []string{"tok_timeout", "tok_network_error"} {
		t.Run(token, func(t *testing.T) {
			f := setupPay(t, 600*time.Millisecond, 200*time.Millisecond)
			inv := openInvoice(t, f)

			r := pay(t, f, inv.ID, token)
			a := attemptOf(t, r)
			if r.Code != http.StatusAccepted || a.Status != "pending" {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}

			reconcile(t, f, a)

			got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil))
			if got.Status != "succeeded" || got.PSPRefID == nil || got.ResolvedAt == nil {
				t.Fatalf("attempt after reconciliation: %+v", got)
			}
			if inv := getInvoice(t, f, inv.ID); inv.Status != "paid" || inv.PaidAt == nil {
				t.Fatalf("invoice after reconciliation: %+v", inv)
			}
			// No double charge: one payment, one attempt, one charge at the provider.
			if countWhere(t, f, "payments", "true") != 1 || countWhere(t, f, "invoice_payment_attempts", "true") != 1 || f.mock.Charges(t) != 1 {
				t.Fatalf("payments=%d attempts=%d provider charges=%d, want 1 each",
					countWhere(t, f, "payments", "true"), countWhere(t, f, "invoice_payment_attempts", "true"), f.mock.Charges(t))
			}
			if rows := transitionRows(t, f, inv.ID); len(rows) != 3 || rows[2] != "open>paid:payment_succeeded" {
				t.Fatalf("transitions = %v", rows)
			}
		})
	}
}

// ---- no transaction is open while the provider is being called ----------------

func TestNoTransactionOrLockIsHeldDuringTheProviderCall(t *testing.T) {
	f := setupPay(t, 2*time.Second, 900*time.Millisecond)
	inv := openInvoice(t, f)
	ctx := context.Background()

	done := make(chan testapi.Response, 1)
	go func() { done <- pay(t, f, inv.ID, "tok_timeout") }()

	// Sample while the call is in flight.
	time.Sleep(250 * time.Millisecond)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		var open int
		err := f.env.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = current_setting('application_name')
			  AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open)
		if err != nil || open != 0 {
			t.Fatalf("%d transactions open during the provider call (err %v)", open, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The invoice row must be free: another request could void it right now
	// (and is refused only because the attempt is pending, not because of a lock).
	tx, err := f.env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE NOWAIT`, inv.ID); err != nil {
		t.Fatalf("the invoice row is locked during the provider call: %v", err)
	}
	_ = tx.Rollback(ctx)
	if r := transition(t, f, f.a.Key, inv.ID, "void"); r.Code != http.StatusConflict || r.ErrorCode(t) != "payment_in_progress" {
		t.Fatalf("void during the call: %d %s", r.Code, r.Body)
	}

	if r := <-done; r.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
}

// ---- idempotency -----------------------------------------------------------

func TestSameKeyReturnsTheSameAttemptWithoutChargingAgain(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	key := uniqueKey()

	first := payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500)
	a1 := attemptOf(t, first)
	if first.Code != 200 || a1.Status != "succeeded" {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	for i := 0; i < 3; i++ {
		again := payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500)
		a2 := attemptOf(t, again)
		// The invoice is paid by now; a replay must still return the original result, not a 409.
		if again.Code != 200 || a2.ID != a1.ID || a2.Status != "succeeded" || !a2.CreatedAt.Equal(a1.CreatedAt) {
			t.Fatalf("replay %d: %d %s", i, again.Code, again.Body)
		}
	}
	if countWhere(t, f, "invoice_payment_attempts", "true") != 1 || countWhere(t, f, "payments", "true") != 1 || f.mock.Charges(t) != 1 {
		t.Fatalf("replays must not create anything: attempts=%d payments=%d charges=%d",
			countWhere(t, f, "invoice_payment_attempts", "true"), countWhere(t, f, "payments", "true"), f.mock.Charges(t))
	}
}

func TestReplayWhilePendingIs202AndThen200OnceResolved(t *testing.T) {
	f := setupPay(t, 500*time.Millisecond, 150*time.Millisecond)
	inv := openInvoice(t, f)
	key := uniqueKey()

	first := payWith(t, f, f.a.Key, inv.ID, key, "tok_timeout", 4500)
	a1 := attemptOf(t, first)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	again := payWith(t, f, f.a.Key, inv.ID, key, "tok_timeout", 4500)
	if a2 := attemptOf(t, again); again.Code != http.StatusAccepted || a2.ID != a1.ID || a2.Status != "pending" {
		t.Fatalf("replay while pending: %d %s", again.Code, again.Body)
	}
	if f.mock.Charges(t) != 1 {
		t.Fatalf("the replay must not call the provider again: %d charges", f.mock.Charges(t))
	}

	reconcile(t, f, a1)
	done := payWith(t, f, f.a.Key, inv.ID, key, "tok_timeout", 4500)
	if a3 := attemptOf(t, done); done.Code != 200 || a3.ID != a1.ID || a3.Status != "succeeded" {
		t.Fatalf("replay after resolution: %d %s", done.Code, done.Body)
	}
}

func TestReusingAKeyWithADifferentRequestIsRejected(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	other := openInvoice(t, f)
	key := uniqueKey()

	if r := payWith(t, f, f.a.Key, inv.ID, key, "tok_card_declined", 4500); r.Code != 200 {
		t.Fatalf("first: %d %s", r.Code, r.Body)
	}
	for name, r := range map[string]testapi.Response{
		"another card token": payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500),
		"another amount":     payWith(t, f, f.a.Key, inv.ID, key, "tok_card_declined", 4499),
		"another invoice":    payWith(t, f, f.a.Key, other.ID, key, "tok_card_declined", 4500),
	} {
		if r.Code != http.StatusUnprocessableEntity || r.ErrorCode(t) != "idempotency_key_reused" {
			t.Errorf("%s: got %d %s", name, r.Code, r.Body)
		}
	}
	if f.mock.Charges(t) != 1 {
		t.Fatalf("rejected requests must not charge: %d", f.mock.Charges(t))
	}
}

func TestAFailedAttemptReplaysAsFailedAndANewKeyRetries(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	key := uniqueKey()

	first := attemptOf(t, payWith(t, f, f.a.Key, inv.ID, key, "tok_card_declined", 4500))
	again := payWith(t, f, f.a.Key, inv.ID, key, "tok_card_declined", 4500)
	if a := attemptOf(t, again); again.Code != 200 || a.ID != first.ID || a.Status != "failed" {
		t.Fatalf("a failed attempt replays as failed, it is not retried: %d %s", again.Code, again.Body)
	}

	// A new key is a new attempt.
	second := attemptOf(t, payWith(t, f, f.a.Key, inv.ID, uniqueKey(), "tok_success", 4500))
	if second.ID == first.ID || second.Status != "succeeded" {
		t.Fatalf("retry with a new key: %+v", second)
	}

	list := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+inv.ID+"/payment_attempts", nil)
	var page struct {
		Data []attemptJSON `json:"data"`
	}
	list.JSON(t, &page)
	if list.Code != 200 || len(page.Data) != 2 || page.Data[0].ID != second.ID || page.Data[1].ID != first.ID {
		t.Fatalf("attempts must be listed newest first: %d %s", list.Code, list.Body)
	}
	if f.mock.Charges(t) != 2 || countWhere(t, f, "payments", "true") != 2 {
		t.Fatalf("charges=%d payments=%d, want 2", f.mock.Charges(t), countWhere(t, f, "payments", "true"))
	}
}

func TestKeysAreScopedPerBusiness(t *testing.T) {
	f := setup(t)
	a := openInvoice(t, f)
	// business B's own open invoice
	b := create(t, f, f.b.Key, f.other)
	transition(t, f, f.b.Key, b.ID, "finalize")

	key := "shared-key"
	if r := payWith(t, f, f.a.Key, a.ID, key, "tok_success", 4500); r.Code != 200 {
		t.Fatalf("A: %d %s", r.Code, r.Body)
	}
	if r := payWith(t, f, f.b.Key, b.ID, key, "tok_success", 4500); r.Code != 200 {
		t.Fatalf("the same key in another business is a different key: %d %s", r.Code, r.Body)
	}
	if f.mock.Charges(t) != 2 {
		t.Fatalf("charges = %d, want 2", f.mock.Charges(t))
	}
}

// ---- validation, ordering and what a rejected request leaves behind ---------

func TestPayRequestValidation(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	do := func(key string, body string) testapi.Response {
		req := httptest.NewRequest(http.MethodPost, "/invoices/"+inv.ID+"/pay", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.a.Key)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		f.env.Handler.ServeHTTP(rec, req)
		return testapi.Response{Code: rec.Code, Body: rec.Body.Bytes()}
	}
	good := `{"card_token":"tok_success","amount_cents":4500}`

	tests := []struct {
		name   string
		key    string
		body   string
		status int
		code   string
		field  string
	}{
		{"missing idempotency key", "", good, 400, "idempotency_key_required", ""},
		{"key too long", strings.Repeat("k", 256), good, 422, "validation_failed", "Idempotency-Key"},
		{"missing card token", "k1", `{"amount_cents":4500}`, 422, "validation_failed", "card_token"},
		{"blank card token", "k1", `{"card_token":"","amount_cents":4500}`, 422, "validation_failed", "card_token"},
		{"card token with a NUL byte", "k1", `{"card_token":"a\u0000b","amount_cents":4500}`, 422, "validation_failed", "card_token"},
		{"card token too long", "k1", `{"card_token":"` + strings.Repeat("t", 101) + `","amount_cents":4500}`, 422, "validation_failed", "card_token"},
		{"zero amount", "k1", `{"card_token":"tok_success","amount_cents":0}`, 422, "validation_failed", "amount_cents"},
		{"negative amount", "k1", `{"card_token":"tok_success","amount_cents":-1}`, 422, "validation_failed", "amount_cents"},
		{"fractional amount", "k1", `{"card_token":"tok_success","amount_cents":45.5}`, 422, "validation_failed", "amount_cents"},
		{"amount as string", "k1", `{"card_token":"tok_success","amount_cents":"4500"}`, 422, "validation_failed", "amount_cents"},
		{"unknown field", "k1", `{"card_token":"tok_success","amount_cents":4500,"currency":"USD"}`, 400, "unknown_field", "currency"},
		{"malformed json", "k1", `{"card_token":`, 400, "invalid_json", ""},
		{"empty body", "k1", ``, 400, "invalid_json", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := do(tt.key, tt.body)
			if r.Code != tt.status || r.ErrorCode(t) != tt.code {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}
			if tt.field != "" && !strings.Contains(r.ErrorMessage(t), tt.field) {
				t.Fatalf("message %q should name %q", r.ErrorMessage(t), tt.field)
			}
		})
	}
	if countWhere(t, f, "invoice_payment_idempotency_keys", "true") != 0 || countWhere(t, f, "payments", "true") != 0 || f.mock.Charges(t) != 0 {
		t.Fatal("rejected requests must leave nothing behind and never charge")
	}
}

func TestPayRequiresJSONAndAuth(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)

	req := httptest.NewRequest(http.MethodPost, "/invoices/"+inv.ID+"/pay", strings.NewReader(`{"card_token":"t","amount_cents":4500}`))
	req.Header.Set("Authorization", "Bearer "+f.a.Key)
	req.Header.Set("Idempotency-Key", "k")
	rec := httptest.NewRecorder()
	f.env.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("no content type: %d", rec.Code)
	}
	if r := payWith(t, f, "", inv.ID, "k", "tok_success", 4500); r.Code != http.StatusUnauthorized {
		t.Fatalf("no api key: %d", r.Code)
	}
}

func TestPayChecksRunInTheDocumentedOrder(t *testing.T) {
	f := setup(t)

	t.Run("states that cannot be paid", func(t *testing.T) {
		for _, state := range []Status{StatusDraft, StatusVoid, StatusPaid} {
			inv := create(t, f, f.a.Key, f.customer)
			forceStatus(t, f, inv.ID, state)
			r := pay(t, f, inv.ID, "tok_success")
			if r.Code != http.StatusConflict || r.ErrorCode(t) != "invalid_transition" || !strings.Contains(r.ErrorMessage(t), string(state)) {
				t.Errorf("%s: got %d %s", state, r.Code, r.Body)
			}
		}
		if f.mock.Charges(t) != 0 {
			t.Fatal("an unpayable invoice must never reach the provider")
		}
	})

	t.Run("uncollectible invoices can be paid", func(t *testing.T) {
		inv := openInvoice(t, f)
		transition(t, f, f.a.Key, inv.ID, "mark_uncollectible")
		r := pay(t, f, inv.ID, "tok_success")
		if r.Code != 200 || getInvoice(t, f, inv.ID).Status != "paid" {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
	})

	t.Run("a wrong amount is rejected and leaves no key, so a corrected retry works", func(t *testing.T) {
		inv := openInvoice(t, f)
		key := uniqueKey()
		r := payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4499)
		if r.Code != http.StatusUnprocessableEntity || r.ErrorCode(t) != "amount_mismatch" {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
		if countWhere(t, f, "invoice_payment_idempotency_keys", "key = $1", key) != 0 {
			t.Fatal("a rejected request must not store its key")
		}
		if r := payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500); r.Code != 200 {
			t.Fatalf("corrected retry with the same key: %d %s", r.Code, r.Body)
		}
	})

	t.Run("another business's invoice and unknown ids are 404", func(t *testing.T) {
		inv := openInvoice(t, f)
		for name, tt := range map[string]struct{ key, id string }{
			"other tenant": {f.b.Key, inv.ID},
			"unknown":      {f.a.Key, uuid.NewString()},
			"malformed":    {f.a.Key, "not-a-uuid"},
		} {
			key := uniqueKey()
			r := payWith(t, f, tt.key, tt.id, key, "tok_success", 4500)
			if r.Code != http.StatusNotFound || r.ErrorCode(t) != "invoice_not_found" {
				t.Errorf("%s: got %d %s", name, r.Code, r.Body)
			}
			if countWhere(t, f, "invoice_payment_idempotency_keys", "key = $1", key) != 0 {
				t.Errorf("%s: a 404 must not store a key", name)
			}
		}
		if getInvoice(t, f, inv.ID).Status != "open" {
			t.Fatal("the other tenant's attempt must not touch the invoice")
		}
	})

	t.Run("a pending attempt blocks a new one, but an old key still replays", func(t *testing.T) {
		g := setupPay(t, time.Hour, 150*time.Millisecond)
		inv := openInvoice(t, g)
		key := uniqueKey()
		first := payWith(t, g, g.a.Key, inv.ID, key, "tok_timeout", 4500)
		if first.Code != http.StatusAccepted {
			t.Fatalf("first: %d %s", first.Code, first.Body)
		}
		r := payWith(t, g, g.a.Key, inv.ID, uniqueKey(), "tok_success", 4500)
		if r.Code != http.StatusConflict || r.ErrorCode(t) != "payment_in_progress" {
			t.Fatalf("second pay: %d %s", r.Code, r.Body)
		}
		if g.mock.Charges(t) != 1 {
			t.Fatalf("charges = %d, want 1", g.mock.Charges(t))
		}
	})
}

// A bad request must be refused without ever waiting for the invoice lock.
func TestInvalidRequestsDoNotTakeTheInvoiceLock(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	ctx := context.Background()

	tx, err := f.env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, inv.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for _, r := range []testapi.Response{
		payWith(t, f, f.a.Key, inv.ID, "", "tok_success", 4500),
		payWith(t, f, f.a.Key, inv.ID, uniqueKey(), "", 4500),
		payWith(t, f, f.a.Key, inv.ID, uniqueKey(), "tok_success", 0),
	} {
		if r.Code != 400 && r.Code != 422 {
			t.Fatalf("got %d %s", r.Code, r.Body)
		}
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("invalid requests waited %v; they must be refused before locking", time.Since(start))
	}

	// A valid request does queue behind the lock.
	done := make(chan testapi.Response, 1)
	go func() { done <- pay(t, f, inv.ID, "tok_success") }()
	select {
	case r := <-done:
		t.Fatalf("a valid pay finished (%d) while the invoice row was locked", r.Code)
	case <-time.After(300 * time.Millisecond):
	}
	_ = tx.Rollback(ctx)
	if r := <-done; r.Code != 200 {
		t.Fatalf("after the lock was released: %d %s", r.Code, r.Body)
	}
}

// ---- concurrency -----------------------------------------------------------

func TestConcurrentPaymentsOnOneInvoiceChargeOnce(t *testing.T) {
	f := setup(t)
	for round := 0; round < 5; round++ {
		inv := openInvoice(t, f)
		const n = 12
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[i] = pay(t, f, inv.ID, "tok_success").Code // a different key each time
			}()
		}
		close(start)
		wg.Wait()

		ok := 0
		for _, c := range codes {
			switch c {
			case 200:
				ok++
			case http.StatusConflict: // payment_in_progress, or already paid
			default:
				t.Fatalf("round %d: unexpected status %d in %v", round, c, codes)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d payments succeeded, want exactly 1: %v", round, ok, codes)
		}
		if got := getInvoice(t, f, inv.ID); got.Status != "paid" {
			t.Fatalf("round %d: invoice %+v", round, got)
		}
		if n := countWhere(t, f, "invoice_payment_attempts", "invoice_id = $1", inv.ID); n != 1 {
			t.Fatalf("round %d: %d attempts for one invoice", round, n)
		}
		if rows := transitionRows(t, f, inv.ID); len(rows) != 3 {
			t.Fatalf("round %d: transitions %v, want a single paid transition", round, rows)
		}
	}
	if f.mock.Charges(t) != 5 || countWhere(t, f, "payments", "true") != 5 {
		t.Fatalf("provider charges=%d payments=%d, want 5 (one per invoice)", f.mock.Charges(t), countWhere(t, f, "payments", "true"))
	}
}

func TestConcurrentPaymentsWhileOneIsPending(t *testing.T) {
	f := setupPay(t, time.Hour, 300*time.Millisecond)
	inv := openInvoice(t, f)

	const n = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = pay(t, f, inv.ID, "tok_timeout").Code
		}()
	}
	close(start)
	wg.Wait()

	accepted, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
		}
	}
	if accepted != 1 || conflicts != n-1 {
		t.Fatalf("codes = %v, want one 202 and %d 409s", codes, n-1)
	}
	if f.mock.Charges(t) != 1 || countWhere(t, f, "payments", "true") != 1 {
		t.Fatalf("charges=%d payments=%d, want 1", f.mock.Charges(t), countWhere(t, f, "payments", "true"))
	}
}

func TestConcurrentRequestsWithTheSameKeyShareOneAttempt(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	key := uniqueKey()

	const n = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]testapi.Response, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500)
		}()
	}
	close(start)
	wg.Wait()

	// A request that arrives while the first one's provider call is still in flight
	// correctly sees the attempt as pending (202); the others see it resolved (200).
	var first string
	for i, r := range results {
		a := attemptOf(t, r)
		if r.Code != 200 && r.Code != http.StatusAccepted {
			t.Fatalf("request %d: %d %s", i, r.Code, r.Body)
		}
		if (r.Code == 200) != a.Resolved() {
			t.Fatalf("request %d: status %d but attempt is %s", i, r.Code, a.Status)
		}
		if first == "" {
			first = a.ID
		}
		if a.ID != first {
			t.Fatalf("requests with one key produced different attempts: %s vs %s", a.ID, first)
		}
	}
	final := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+first, nil))
	if final.Status != "succeeded" {
		t.Fatalf("the single attempt should end succeeded, got %s", final.Status)
	}
	if f.mock.Charges(t) != 1 || countWhere(t, f, "invoice_payment_attempts", "true") != 1 {
		t.Fatalf("charges=%d attempts=%d, want 1", f.mock.Charges(t), countWhere(t, f, "invoice_payment_attempts", "true"))
	}
}

func TestSameKeyOnDifferentInvoicesRacingNeverDoubleBooks(t *testing.T) {
	f := setup(t)
	a, b := openInvoice(t, f), openInvoice(t, f)
	key := uniqueKey()

	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, id := range []string{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = payWith(t, f, f.a.Key, id, key, "tok_success", 4500).Code
		}()
	}
	close(start)
	wg.Wait()

	// They hold different invoice locks, so only the key's primary key arbitrates.
	if (codes[0] != 200 || codes[1] != 422) && (codes[0] != 422 || codes[1] != 200) {
		t.Fatalf("codes = %v, want one 200 and one 422 idempotency_key_reused", codes)
	}
	if f.mock.Charges(t) != 1 {
		t.Fatalf("charges = %d, want 1", f.mock.Charges(t))
	}
}

func TestPayRacingVoidNeverLeavesAPaymentOnAVoidInvoice(t *testing.T) {
	f := setup(t)
	for round := 0; round < 12; round++ {
		inv := openInvoice(t, f)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var payCode, voidCode int
		wg.Add(2)
		go func() { defer wg.Done(); <-start; payCode = pay(t, f, inv.ID, "tok_success").Code }()
		go func() { defer wg.Done(); <-start; voidCode = transition(t, f, f.a.Key, inv.ID, "void").Code }()
		close(start)
		wg.Wait()

		status := getInvoice(t, f, inv.ID).Status
		switch {
		case status == "void" && payCode == 409 && voidCode == 200:
		case status == "paid" && payCode == 200 && voidCode == 409:
		default:
			t.Fatalf("round %d: invoice %s, pay %d, void %d", round, status, payCode, voidCode)
		}
		if status == "void" && countWhere(t, f, "invoice_payment_attempts", "invoice_id = $1", inv.ID) != 0 {
			t.Fatalf("round %d: a void invoice has a payment attempt", round)
		}
	}
}

// Deterministic: while a request holds the invoice lock, a payment waits, and
// is judged against the state the lock holder leaves behind.
func TestPayWaitsForTheInvoiceLock(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	ctx := context.Background()

	tx, err := f.env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, inv.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan testapi.Response, 1)
	go func() { done <- pay(t, f, inv.ID, "tok_success") }()
	select {
	case r := <-done:
		t.Fatalf("pay finished (%d) while the invoice was locked", r.Code)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `UPDATE invoices SET status = 'void' WHERE id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Code != http.StatusConflict || r.ErrorCode(t) != "invalid_transition" || !strings.Contains(r.ErrorMessage(t), "void") {
		t.Fatalf("got %d %s, want 409 invalid_transition naming void", r.Code, r.Body)
	}
	if f.mock.Charges(t) != 0 {
		t.Fatal("a payment judged invalid must never be sent to the provider")
	}
}

func TestVoidAndMarkUncollectibleRefuseWhilePending(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	inv := openInvoice(t, f)
	pay(t, f, inv.ID, "tok_timeout") // pending

	for _, action := range []string{"void", "mark_uncollectible"} {
		r := transition(t, f, f.a.Key, inv.ID, action)
		if r.Code != http.StatusConflict || r.ErrorCode(t) != "payment_in_progress" {
			t.Errorf("%s: got %d %s", action, r.Code, r.Body)
		}
	}
	assertStatus(t, f, inv.ID, StatusOpen)

	// Once the attempt has failed, the invoice can be taken out of collection again.
	forceFail := f.svc.ResolvePayment
	var ref uuid.UUID
	if err := f.env.Pool.QueryRow(context.Background(),
		`SELECT payment_ref_id FROM invoice_payment_attempts WHERE invoice_id = $1`, inv.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if err := forceFail(context.Background(), ref, psp.Outcome{Status: psp.Failed, Code: psp.CodeCardDeclined}); err != nil {
		t.Fatal(err)
	}
	if r := transition(t, f, f.a.Key, inv.ID, "void"); r.Code != 200 {
		t.Fatalf("void after the attempt failed: %d %s", r.Code, r.Body)
	}
}

// ---- resolution --------------------------------------------------------------

func TestResolvePaymentAppliesOnceWhoeverGetsThereFirst(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	for round := 0; round < 8; round++ {
		inv := openInvoice(t, f)
		a := attemptOf(t, pay(t, f, inv.ID, "tok_timeout"))
		ref := f.paymentRef(t, a.ID)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := f.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-1"}); err != nil {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if rows := transitionRows(t, f, inv.ID); len(rows) != 3 {
			t.Fatalf("round %d: transitions = %v, want exactly one paid transition", round, rows)
		}
		if got := getInvoice(t, f, inv.ID); got.Status != "paid" {
			t.Fatalf("round %d: %+v", round, got)
		}
		// A late contradicting answer changes nothing.
		if err := f.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Failed, Code: psp.CodeCardDeclined}); err != nil {
			t.Fatal(err)
		}
		if got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil)); got.Status != "succeeded" {
			t.Fatalf("round %d: a resolved attempt changed to %s", round, got.Status)
		}
	}
}

func TestResolvePaymentRefusesUncertainOutcomes(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_timeout"))
	ref := f.paymentRef(t, a.ID)

	for name, o := range map[string]psp.Outcome{
		"unknown":    {Status: psp.Unknown, Cause: errors.New("timeout")},
		"processing": {Status: psp.Processing},
		"not found":  {Status: psp.NotFound},
	} {
		if err := f.svc.ResolvePayment(context.Background(), ref, o); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil)); got.Status != "pending" {
		t.Fatalf("the attempt must stay pending, got %s", got.Status)
	}
}

// If recording the provider's answer fails, the caller still gets a safe 202 and
// the attempt stays pending for the reconciler.
func TestFailureToRecordTheAnswerLeavesTheAttemptPending(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	ctx := context.Background()
	if _, err := f.env.Pool.Exec(ctx, `
		CREATE FUNCTION fail_paid() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN IF NEW.to_status = 'paid' THEN RAISE EXCEPTION 'simulated failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_paid BEFORE INSERT ON invoice_transitions
		FOR EACH ROW EXECUTE FUNCTION fail_paid();`); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)

	r := pay(t, f, inv.ID, "tok_success")
	a := attemptOf(t, r)
	if r.Code != http.StatusAccepted || a.Status != "pending" {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	if !strings.Contains(logs.String(), "could not record the provider's answer") {
		t.Fatalf("the failure must be logged: %s", logs.String())
	}
	if getInvoice(t, f, inv.ID).Status != "open" || countWhere(t, f, "payments", "status = 'pending'") != 1 {
		t.Fatal("nothing may be half-recorded")
	}

	// The provider did charge; asking it settles everything.
	if _, err := f.env.Pool.Exec(ctx, `DROP TRIGGER fail_paid ON invoice_transitions`); err != nil {
		t.Fatal(err)
	}
	reconcile(t, f, a)
	if getInvoice(t, f, inv.ID).Status != "paid" {
		t.Fatal("the invoice should be paid after reconciliation")
	}
	if f.mock.Charges(t) != 1 {
		t.Fatalf("charges = %d, want 1", f.mock.Charges(t))
	}
}

// Once the attempt is committed the work completes even if the HTTP caller hangs up.
func TestAClientThatHangsUpDoesNotAbandonThePayment(t *testing.T) {
	f := setupPay(t, 400*time.Millisecond, 3*time.Second)
	inv := openInvoice(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/invoices/"+inv.ID+"/pay",
		strings.NewReader(`{"card_token":"tok_timeout","amount_cents":4500}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.a.Key)
	req.Header.Set("Idempotency-Key", uniqueKey())

	done := make(chan struct{})
	go func() { f.env.Handler.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel() // the caller gives up mid-call
	<-done

	// The provider call was allowed to finish, and the answer was recorded.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if getInvoice(t, f, inv.ID).Status == "paid" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("the payment was abandoned when the caller hung up: invoice %+v", getInvoice(t, f, inv.ID))
}

// ---- read endpoints --------------------------------------------------------

func TestAttemptEndpointsAreTenantScoped(t *testing.T) {
	f := setup(t)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_success"))

	r := f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil)
	if got := attemptOf(t, r); r.Code != 200 || got.ID != a.ID || got.InvoiceID != inv.ID {
		t.Fatalf("owner: %d %s", r.Code, r.Body)
	}
	for name, tt := range map[string]struct{ key, path string }{
		"another business's attempt": {f.b.Key, "/payment_attempts/" + a.ID},
		"unknown attempt":            {f.a.Key, "/payment_attempts/" + uuid.NewString()},
		"malformed attempt id":       {f.a.Key, "/payment_attempts/nope"},
	} {
		r := f.env.Do(t, tt.key, http.MethodGet, tt.path, nil)
		if r.Code != 404 || r.ErrorCode(t) != "payment_attempt_not_found" {
			t.Errorf("%s: got %d %s", name, r.Code, r.Body)
		}
	}
	for name, tt := range map[string]struct{ key, path string }{
		"another business's invoice": {f.b.Key, "/invoices/" + inv.ID + "/payment_attempts"},
		"unknown invoice":            {f.a.Key, "/invoices/" + uuid.NewString() + "/payment_attempts"},
		"malformed invoice id":       {f.a.Key, "/invoices/nope/payment_attempts"},
	} {
		r := f.env.Do(t, tt.key, http.MethodGet, tt.path, nil)
		if r.Code != 404 || r.ErrorCode(t) != "invoice_not_found" {
			t.Errorf("%s: got %d %s", name, r.Code, r.Body)
		}
	}
	if r := f.env.Do(t, "", http.MethodGet, "/payment_attempts/"+a.ID, nil); r.Code != 401 {
		t.Errorf("no key: %d", r.Code)
	}

	// An invoice with no attempts lists an empty array, not null.
	fresh := openInvoice(t, f)
	if r := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+fresh.ID+"/payment_attempts", nil); !strings.Contains(string(r.Body), `"data":[]`) {
		t.Fatalf("got %s", r.Body)
	}
}

// ---- invariants ----------------------------------------------------------------

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
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// The card token is handed to the provider and then forgotten: it appears in no
// table of ours (checked across every row after exercising every token) and in no log line.
func TestTheCardTokenIsNeverStoredOrLogged(t *testing.T) {
	logs := captureLogs(t)
	f := setupPay(t, 500*time.Millisecond, 200*time.Millisecond)

	tokens := []string{"tok_success", "tok_insufficient_funds", "tok_card_declined", "tok_timeout", "tok_network_error"}
	var attempts []attemptJSON
	for _, token := range tokens {
		inv := openInvoice(t, f)
		attempts = append(attempts, attemptOf(t, pay(t, f, inv.ID, token)))
		pay(t, f, inv.ID, token) // a second try is refused or answered, and must not leak either
	}
	// A failure on our side must not echo the token into logs either.
	payWith(t, f, f.a.Key, uuid.NewString(), uniqueKey(), "tok_success", 4500)
	reconcile(t, f, attempts[3])

	rows, err := f.env.Pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	for _, table := range tables {
		var hits int
		q := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE row_to_json(t)::text ILIKE '%%tok\_%%'`, table)
		if err := f.env.Pool.QueryRow(context.Background(), q).Scan(&hits); err != nil {
			t.Fatal(err)
		}
		if hits != 0 {
			t.Errorf("table %s holds a card token in %d rows", table, hits)
		}
	}
	for _, token := range tokens {
		if strings.Contains(logs.String(), token) {
			t.Errorf("log output contains %s", token)
		}
	}
	if len(tables) < 8 {
		t.Fatalf("only scanned %d tables: %v", len(tables), tables)
	}
}

func TestPaymentSchemaConstraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inv := openInvoice(t, f)
	other := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_card_declined"))
	_ = other

	mkPayment := func() uuid.UUID {
		ref := uuid.New()
		if _, err := f.env.Pool.Exec(ctx, `INSERT INTO payments (payment_ref_id, amount_cents) VALUES ($1, 100)`, ref); err != nil {
			t.Fatal(err)
		}
		return ref
	}

	tests := []struct {
		name string
		run  func() error
		code string
	}{
		{"one pending attempt per invoice", func() error {
			if _, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id) VALUES ($1, $2, $3)`, uuid.New(), inv.ID, mkPayment()); err != nil {
				t.Fatal(err)
			}
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id) VALUES ($1, $2, $3)`, uuid.New(), inv.ID, mkPayment())
			return err
		}, "23505"},
		{"a payment belongs to one attempt", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id)
				SELECT $1, $2, payment_ref_id FROM invoice_payment_attempts WHERE id = $3`, uuid.New(), other.ID, a.ID)
			return err
		}, "23505"},
		{"failed attempts need a failure code", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id, status, resolved_at) VALUES ($1, $2, $3, 'failed', now())`, uuid.New(), other.ID, mkPayment())
			return err
		}, "23514"},
		{"pending attempts have no resolution time", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id, resolved_at) VALUES ($1, $2, $3, now())`, uuid.New(), other.ID, mkPayment())
			return err
		}, "23514"},
		{"attempt status must be known", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id, status, resolved_at) VALUES ($1, $2, $3, 'refunded', now())`, uuid.New(), other.ID, mkPayment())
			return err
		}, "23514"},
		{"an attempt needs a real payment", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id) VALUES ($1, $2, $3)`, uuid.New(), other.ID, uuid.New())
			return err
		}, "23503"},
		{"latest attempt must belong to the same invoice", func() error {
			_, err := f.env.Pool.Exec(ctx, `UPDATE invoices SET latest_payment_attempt_id = $2 WHERE id = $1`, other.ID, a.ID)
			return err
		}, "23503"},
		{"an idempotency key points at one attempt", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_idempotency_keys (business_id, key, request_hash, attempt_id)
				SELECT business_id, 'another', request_hash, attempt_id FROM invoice_payment_idempotency_keys LIMIT 1`)
			return err
		}, "23505"},
		{"request hash is a SHA-256", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_idempotency_keys (business_id, key, request_hash, attempt_id) VALUES ($1, 'k', '\x00', $2)`, f.a.BusinessID, a.ID)
			return err
		}, "23514"},
		{"keys are 1 to 255 characters", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_payment_idempotency_keys (business_id, key, request_hash, attempt_id) SELECT $1, '', request_hash, $2 FROM invoice_payment_idempotency_keys LIMIT 1`, f.a.BusinessID, a.ID)
			return err
		}, "23514"},
		{"transitions reference real attempts", func() error {
			_, err := f.env.Pool.Exec(ctx, `INSERT INTO invoice_transitions (id, invoice_id, to_status, reason, attempt_id) VALUES ($1, $2, 'paid', 'x', $3)`, uuid.New(), other.ID, uuid.New())
			return err
		}, "23503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("got %v, want SQLSTATE %s", err, tt.code)
			}
		})
	}

	t.Run("the stored request hash is 32 bytes and not the token", func(t *testing.T) {
		var n int
		err := f.env.Pool.QueryRow(ctx, `SELECT count(*) FROM invoice_payment_idempotency_keys WHERE octet_length(request_hash) = 32`).Scan(&n)
		if err != nil || n < 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})
}

func TestPayRequestHashIsStableAndDistinguishesRequests(t *testing.T) {
	inv := uuid.New()
	base := payRequestHash(inv, "tok_success", 4500)
	if !bytes.Equal(base, payRequestHash(inv, "tok_success", 4500)) || len(base) != 32 {
		t.Fatal("the hash must be deterministic and 32 bytes")
	}
	for name, other := range map[string][]byte{
		"token":   payRequestHash(inv, "tok_card_declined", 4500),
		"amount":  payRequestHash(inv, "tok_success", 4501),
		"invoice": payRequestHash(uuid.New(), "tok_success", 4500),
		// shifting characters between fields must not collide
		"boundary": payRequestHash(inv, "tok_succes", 4500),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("a different %s hashed the same", name)
		}
	}
}

// Only the invoice's latest attempt may mark it paid. Normal flows never leave a
// stale attempt behind, so the state is built by hand: the money was received, the
// payment is recorded as succeeded, but the invoice is left alone and the problem is logged.
func TestASuccessForANonLatestAttemptDoesNotMarkTheInvoicePaid(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	inv := openInvoice(t, f)
	stale := attemptOf(t, pay(t, f, inv.ID, "tok_timeout")) // pending
	staleRef := f.paymentRef(t, stale.ID)
	ctx := context.Background()

	// Point the invoice at a different (already failed) attempt of its own.
	other := uuid.New()
	ref := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO payments (payment_ref_id, amount_cents, status, failure_code, resolved_at) VALUES ($1, 4500, 'failed', 'card_declined', now())`, []any{ref}},
		// the one-pending index only constrains pending rows, so a failed sibling is allowed
		{`INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id, status, failure_code, resolved_at) VALUES ($1, $2, $3, 'failed', 'card_declined', now())`, []any{other, inv.ID, ref}},
		{`UPDATE invoices SET latest_payment_attempt_id = $2 WHERE id = $1`, []any{inv.ID, other}},
	} {
		if _, err := f.env.Pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	logs := captureLogs(t)

	if err := f.svc.ResolvePayment(ctx, staleRef, psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-late"}); err != nil {
		t.Fatal(err)
	}
	if got := getInvoice(t, f, inv.ID); got.Status != "open" || got.PaidAt != nil {
		t.Fatalf("a non-latest attempt must not mark the invoice paid: %+v", got)
	}
	if got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+stale.ID, nil)); got.Status != "succeeded" || got.PSPRefID == nil {
		t.Fatalf("the received payment must still be recorded: %+v", got)
	}
	if !strings.Contains(logs.String(), "needs attention") {
		t.Fatalf("money received for an unpayable invoice must be logged loudly: %s", logs.String())
	}
}

// An attempt and its payment are resolved together, in one transaction. If they
// ever disagree (someone edited the database), resolution refuses and changes nothing.
func TestResolvePaymentRefusesWhenAttemptAndPaymentDisagree(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_timeout"))
	ref := f.paymentRef(t, a.ID)
	ctx := context.Background()

	// Resolve the attempt behind the payment's back.
	if _, err := f.env.Pool.Exec(ctx,
		`UPDATE invoice_payment_attempts SET status = 'failed', failure_code = 'card_declined', resolved_at = now() WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	err := f.svc.ResolvePayment(ctx, ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-1"})
	if err == nil {
		t.Fatal("a diverged attempt must be reported, not papered over")
	}
	if n := countWhere(t, f, "payments", "payment_ref_id = $1 AND status = 'pending'", ref); n != 1 {
		t.Fatal("the failed resolution must roll back, leaving the payment pending")
	}
	if got := getInvoice(t, f, inv.ID); got.Status != "open" {
		t.Fatalf("invoice changed: %+v", got)
	}
}

// The opposite divergence is reported loudly too, not ignored.
func TestResolvePaymentReportsAPendingAttemptBehindAResolvedPayment(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_timeout"))
	ref := f.paymentRef(t, a.ID)
	if _, err := f.env.Pool.Exec(context.Background(),
		`UPDATE payments SET status = 'failed', failure_code = 'card_declined', resolved_at = now() WHERE payment_ref_id = $1`, ref); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)

	if err := f.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "its attempt is still pending") {
		t.Fatalf("expected an error log about the divergence: %s", logs.String())
	}
}

// A normal race between two resolvers must not raise that alarm.
func TestRacingResolversDoNotFalselyReportADivergence(t *testing.T) {
	f := setupPay(t, time.Hour, 150*time.Millisecond)
	logs := captureLogs(t)
	for round := 0; round < 6; round++ {
		inv := openInvoice(t, f)
		ref := f.paymentRef(t, attemptOf(t, pay(t, f, inv.ID, "tok_timeout")).ID)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = f.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "p"})
			}()
		}
		close(start)
		wg.Wait()
	}
	if strings.Contains(logs.String(), "needs attention") {
		t.Fatalf("a normal race was reported as a divergence: %s", logs.String())
	}
}
