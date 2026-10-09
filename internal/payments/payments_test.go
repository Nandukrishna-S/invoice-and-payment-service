package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/testdb"
)

var ctx = context.Background()

func newPayment(t *testing.T, pool *pgxpool.Pool, amount int64) Payment {
	t.Helper()
	p, err := NewService().Create(ctx, pool, amount)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateMakesAPendingPayment(t *testing.T) {
	pool := testdb.New(t)
	p := newPayment(t, pool, 4500)

	if p.Status != StatusPending || p.AmountCents != 4500 || p.PSPRefID != nil || p.FailureCode != nil || p.ResolvedAt != nil || p.CreatedAt.IsZero() {
		t.Fatalf("unexpected payment: %+v", p)
	}
	if p.RefID.Version() != 7 {
		t.Fatalf("ref %s must be a UUIDv7", p.RefID)
	}
	got, err := NewService().Get(ctx, pool, p.RefID)
	if err != nil || got.RefID != p.RefID {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := NewService().Get(ctx, pool, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a missing payment: %v", err)
	}
}

func TestCreateRejectsNonPositiveAmounts(t *testing.T) {
	pool := testdb.New(t)
	for _, amount := range []int64{0, -1, -4500} {
		if _, err := NewService().Create(ctx, pool, amount); err == nil {
			t.Errorf("amount %d should be rejected", amount)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected creates stored %d rows, %v", n, err)
	}
}

func TestResolveRecordsDefinitiveOutcomes(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService()

	ok := newPayment(t, pool, 100)
	applied, err := svc.Resolve(ctx, pool, ok.RefID, psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-123"})
	if err != nil || !applied {
		t.Fatalf("resolve success: applied=%v err=%v", applied, err)
	}
	got, _ := svc.Get(ctx, pool, ok.RefID)
	if got.Status != StatusSucceeded || got.PSPRefID == nil || *got.PSPRefID != "psp-123" || got.FailureCode != nil || got.ResolvedAt == nil {
		t.Fatalf("succeeded payment: %+v", got)
	}

	bad := newPayment(t, pool, 100)
	applied, err = svc.Resolve(ctx, pool, bad.RefID, psp.Outcome{Status: psp.Failed, Code: psp.CodeInsufficientFunds})
	if err != nil || !applied {
		t.Fatalf("resolve failure: applied=%v err=%v", applied, err)
	}
	got, _ = svc.Get(ctx, pool, bad.RefID)
	if got.Status != StatusFailed || got.FailureCode == nil || *got.FailureCode != "insufficient_funds" || got.PSPRefID != nil || got.ResolvedAt == nil {
		t.Fatalf("failed payment: %+v", got)
	}
}

// Only a definitive provider answer may resolve a payment; uncertainty must
// leave it pending, so a timeout can never be recorded as a failure.
func TestResolveRefusesAnythingNotDefinitive(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService()
	p := newPayment(t, pool, 100)

	for name, o := range map[string]psp.Outcome{
		"unknown (timeout, 5xx, dropped connection)": {Status: psp.Unknown, Cause: errors.New("deadline exceeded")},
		"still processing":                           {Status: psp.Processing},
		"provider has no record":                     {Status: psp.NotFound},
		"success without a reference":                {Status: psp.Succeeded},
		"failure without a code":                     {Status: psp.Failed},
		"empty outcome":                              {},
		"a made-up status":                           {Status: "failed-ish", Code: "card_declined"},
	} {
		applied, err := svc.Resolve(ctx, pool, p.RefID, o)
		if applied || !errors.Is(err, ErrNotDefinitive) {
			t.Errorf("%s: applied=%v err=%v, want ErrNotDefinitive", name, applied, err)
		}
	}
	got, _ := svc.Get(ctx, pool, p.RefID)
	if got.Status != StatusPending || got.ResolvedAt != nil {
		t.Fatalf("the payment must still be pending: %+v", got)
	}
}

// A payment resolves once. Whoever gets there second, in either direction,
// changes nothing.
func TestAResolvedPaymentNeverChanges(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService()
	succeeded := psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-1"}
	failed := psp.Outcome{Status: psp.Failed, Code: psp.CodeCardDeclined}

	for name, order := range map[string][2]psp.Outcome{
		"success then failure": {succeeded, failed},
		"failure then success": {failed, succeeded},
		"success twice":        {succeeded, {Status: psp.Succeeded, PSPRef: "psp-2"}},
	} {
		p := newPayment(t, pool, 100)
		if applied, err := svc.Resolve(ctx, pool, p.RefID, order[0]); err != nil || !applied {
			t.Fatalf("%s: first resolve: %v %v", name, applied, err)
		}
		before, _ := svc.Get(ctx, pool, p.RefID)

		applied, err := svc.Resolve(ctx, pool, p.RefID, order[1])
		if err != nil || applied {
			t.Errorf("%s: second resolve applied=%v err=%v, want a quiet no-op", name, applied, err)
		}
		after, _ := svc.Get(ctx, pool, p.RefID)
		if after.Status != before.Status || !ptrEq(after.PSPRefID, before.PSPRefID) || !ptrEq(after.FailureCode, before.FailureCode) || !after.ResolvedAt.Equal(*before.ResolvedAt) {
			t.Errorf("%s: payment changed from %+v to %+v", name, before, after)
		}
	}
}

func ptrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestResolvingAnUnknownPaymentIsANoOp(t *testing.T) {
	pool := testdb.New(t)
	applied, err := NewService().Resolve(ctx, pool, uuid.New(), psp.Outcome{Status: psp.Succeeded, PSPRef: "x"})
	if err != nil || applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
}

// The inline pay call and the reconciler can answer at the same moment; exactly
// one of them may win, and the stored result must be the winner's.
func TestConcurrentResolversApplyExactlyOnce(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService()

	for round := 0; round < 10; round++ {
		p := newPayment(t, pool, 100)
		const n = 16
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]bool, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o := psp.Outcome{Status: psp.Succeeded, PSPRef: "winner"}
				if i%2 == 1 {
					o = psp.Outcome{Status: psp.Failed, Code: psp.CodeCardDeclined}
				}
				<-start
				applied, err := svc.Resolve(ctx, pool, p.RefID, o)
				if err != nil {
					t.Error(err)
				}
				results[i] = applied
			}()
		}
		close(start)
		wg.Wait()

		wins := 0
		for _, r := range results {
			if r {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d resolvers applied, want exactly 1", round, wins)
		}
		got, _ := svc.Get(ctx, pool, p.RefID)
		if got.Status == StatusPending || (got.Status == StatusSucceeded) != (got.PSPRefID != nil) {
			t.Fatalf("round %d: inconsistent final state %+v", round, got)
		}
	}
}

// The schema re-checks the rules, so even code that bypasses the service can't
// store an impossible payment.
func TestDatabaseConstraints(t *testing.T) {
	pool := testdb.New(t)
	tests := []struct {
		name string
		sql  string
		code string
	}{
		{"amount must be positive", `INSERT INTO payments (payment_ref_id, amount_cents) VALUES (gen_random_uuid(), 0)`, "23514"},
		{"status must be known", `INSERT INTO payments (payment_ref_id, amount_cents, status, resolved_at) VALUES (gen_random_uuid(), 1, 'refunded', now())`, "23514"},
		{"pending has no resolution time", `INSERT INTO payments (payment_ref_id, amount_cents, resolved_at) VALUES (gen_random_uuid(), 1, now())`, "23514"},
		{"resolved has a resolution time", `INSERT INTO payments (payment_ref_id, amount_cents, status, psp_ref_id) VALUES (gen_random_uuid(), 1, 'succeeded', 'r')`, "23514"},
		{"failed needs a failure code", `INSERT INTO payments (payment_ref_id, amount_cents, status, resolved_at) VALUES (gen_random_uuid(), 1, 'failed', now())`, "23514"},
		{"a failure code needs failed", `INSERT INTO payments (payment_ref_id, amount_cents, status, failure_code, psp_ref_id, resolved_at) VALUES (gen_random_uuid(), 1, 'succeeded', 'card_declined', 'r', now())`, "23514"},
		{"failure code must be known", `INSERT INTO payments (payment_ref_id, amount_cents, status, failure_code, resolved_at) VALUES (gen_random_uuid(), 1, 'failed', 'fraud', now())`, "23514"},
		{"succeeded needs a psp reference", `INSERT INTO payments (payment_ref_id, amount_cents, status, resolved_at) VALUES (gen_random_uuid(), 1, 'succeeded', now())`, "23514"},
		{"a psp reference needs succeeded", `INSERT INTO payments (payment_ref_id, amount_cents, psp_ref_id) VALUES (gen_random_uuid(), 1, 'r')`, "23514"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tt.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("got %v, want SQLSTATE %s", err, tt.code)
			}
		})
	}

	t.Run("the payment_ref_id is unique", func(t *testing.T) {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO payments (payment_ref_id, amount_cents) VALUES ($1, 1)`, id); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `INSERT INTO payments (payment_ref_id, amount_cents) VALUES ($1, 1)`, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("got %v, want a unique violation", err)
		}
	})
}

// The card token is never stored: the table has no column that could hold one.
func TestThereIsNoColumnForTheCardToken(t *testing.T) {
	pool := testdb.New(t)
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'payments' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	want := "payment_ref_id amount_cents status psp_ref_id failure_code created_at resolved_at"
	if got := strings.Join(cols, " "); got != want {
		t.Fatalf("columns = %q, want %q (no room for a card token)", got, want)
	}
}

// The reconciler's scan must stay small: an index that covers only pending rows.
func TestPendingIndexCoversOnlyInFlightPayments(t *testing.T) {
	pool := testdb.New(t)
	var def string
	err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'payments_pending_created_at'`).Scan(&def)
	if err != nil {
		t.Fatalf("index missing: %v", err)
	}
	for _, want := range []string{"created_at", "status = 'pending'"} {
		if !strings.Contains(def, want) {
			t.Fatalf("index definition %q should contain %q", def, want)
		}
	}
}

// The reconciler's scan: only pending payments, only old enough, oldest first, with a
// cursor that never skips or repeats a row (even ones created at the same instant).
func TestPendingPage(t *testing.T) {
	pool := testdb.New(t)
	svc := NewService()
	age := func(ref uuid.UUID, d string) {
		if _, err := pool.Exec(ctx, `UPDATE payments SET created_at = now() - $2::interval WHERE payment_ref_id = $1`, ref, d); err != nil {
			t.Fatal(err)
		}
	}
	oldest := newPayment(t, pool, 100)
	age(oldest.RefID, "3 hours")
	older := newPayment(t, pool, 100)
	age(older.RefID, "2 hours")
	young := newPayment(t, pool, 100) // just created
	resolved := newPayment(t, pool, 100)
	age(resolved.RefID, "5 hours")
	if _, err := svc.Resolve(ctx, pool, resolved.RefID, psp.Outcome{Status: psp.Succeeded, PSPRef: "x"}); err != nil {
		t.Fatal(err)
	}

	page, err := svc.PendingPage(ctx, pool, time.Minute, PendingCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].RefID != oldest.RefID || page[1].RefID != older.RefID {
		t.Fatalf("got %+v, want the two old pending payments, oldest first (not the young or resolved ones)", page)
	}
	for _, p := range page {
		if p.RefID == young.RefID || p.RefID == resolved.RefID {
			t.Fatalf("%s must not be listed", p.RefID)
		}
	}

	// Paging walks the same list exactly once, including rows with identical timestamps.
	same := make([]uuid.UUID, 5)
	for i := range same {
		same[i] = newPayment(t, pool, 100).RefID
		age(same[i], "1 hour")
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET created_at = (SELECT created_at FROM payments WHERE payment_ref_id = $1) WHERE payment_ref_id = ANY($2)`, same[0], same); err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]int{}
	var cursor PendingCursor
	for {
		page, err := svc.PendingPage(ctx, pool, time.Minute, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, p := range page {
			seen[p.RefID]++
		}
		cursor = page[len(page)-1].Cursor()
	}
	if len(seen) != 7 {
		t.Fatalf("paging saw %d payments, want 7", len(seen))
	}
	for ref, n := range seen {
		if n != 1 {
			t.Errorf("%s listed %d times", ref, n)
		}
	}
}
