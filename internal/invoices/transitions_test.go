package invoices

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"invoice-and-payment-service/internal/testapi"
)

func transition(t *testing.T, f fixture, key, id, action string) testapi.Response {
	t.Helper()
	return f.env.Do(t, key, http.MethodPost, "/invoices/"+id+"/"+action, nil)
}

// forceStatus puts an invoice in a state directly, as later checkpoints (pay) will.
func forceStatus(t *testing.T, f fixture, id string, status Status) {
	t.Helper()
	q := `UPDATE invoices SET status = $2 WHERE id = $1`
	if status == StatusPaid {
		q = `UPDATE invoices SET status = $2, paid_at = now() WHERE id = $1`
	}
	if _, err := f.env.Pool.Exec(context.Background(), q, id, status); err != nil {
		t.Fatal(err)
	}
}

func transitionRows(t *testing.T, f fixture, id string) []string {
	t.Helper()
	rows, err := f.env.Pool.Query(context.Background(),
		`SELECT coalesce(from_status, '-') || '>' || to_status || ':' || reason
		 FROM invoice_transitions WHERE invoice_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// Every (state, action) pair, against the table in DESIGN.md section 2.
func TestEveryStateAndActionThroughTheAPI(t *testing.T) {
	f := setup(t)
	// result is the new status, or "" when the action must be rejected.
	want := map[string]map[Status]Status{
		"finalize": {
			StatusDraft: StatusOpen,
		},
		"void": {
			StatusDraft: StatusVoid, StatusOpen: StatusVoid, StatusUncollectible: StatusVoid,
		},
		"mark_uncollectible": {
			StatusOpen: StatusUncollectible,
		},
	}
	for action, outcomes := range want {
		for _, from := range allStatuses {
			t.Run(fmt.Sprintf("%s from %s", action, from), func(t *testing.T) {
				inv := create(t, f, f.a.Key, f.customer)
				forceStatus(t, f, inv.ID, from)
				before := transitionRows(t, f, inv.ID)

				resp := transition(t, f, f.a.Key, inv.ID, action)
				to, ok := outcomes[from]
				if !ok {
					if resp.Code != http.StatusConflict || resp.ErrorCode(t) != "invalid_transition" ||
						!strings.Contains(resp.ErrorMessage(t), string(from)) {
						t.Fatalf("got %d %s, want 409 invalid_transition naming %q", resp.Code, resp.Body, from)
					}
					assertStatus(t, f, inv.ID, from)
					if got := transitionRows(t, f, inv.ID); len(got) != len(before) {
						t.Fatalf("a rejected action must not write a transition: %v", got)
					}
					return
				}
				var got invoiceJSON
				resp.JSON(t, &got)
				if resp.Code != http.StatusOK || got.Status != string(to) {
					t.Fatalf("got %d %s, want 200 with status %s", resp.Code, resp.Body, to)
				}
				assertStatus(t, f, inv.ID, to)
				rows := transitionRows(t, f, inv.ID)
				wantRow := fmt.Sprintf("%s>%s:", from, to)
				if len(rows) != len(before)+1 || !strings.HasPrefix(rows[len(rows)-1], wantRow) {
					t.Fatalf("expected a new %q transition row, got %v", wantRow, rows)
				}
			})
		}
	}
}

func assertStatus(t *testing.T, f fixture, id string, want Status) {
	t.Helper()
	var got Status
	if err := f.env.Pool.QueryRow(context.Background(), `SELECT status FROM invoices WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stored status = %s, want %s", got, want)
	}
}

func TestFinalizeAssignsConsecutiveNumbers(t *testing.T) {
	f := setup(t)
	draft := create(t, f, f.a.Key, f.customer)
	if draft.InvoiceSequenceNumber != nil {
		t.Fatal("a draft must not hold a number")
	}

	first, second := create(t, f, f.a.Key, f.customer), create(t, f, f.a.Key, f.customer)
	var got1, got2 invoiceJSON
	r1 := transition(t, f, f.a.Key, first.ID, "finalize")
	r1.JSON(t, &got1)
	r2 := transition(t, f, f.a.Key, second.ID, "finalize")
	r2.JSON(t, &got2)

	if got1.InvoiceSequenceNumber == nil || *got1.InvoiceSequenceNumber != "INV-000001/26-27" ||
		got2.InvoiceSequenceNumber == nil || *got2.InvoiceSequenceNumber != "INV-000002/26-27" {
		t.Fatalf("numbers = %v, %v", got1.InvoiceSequenceNumber, got2.InvoiceSequenceNumber)
	}
	if got1.Status != "open" || len(got1.LineItems) != 1 || got1.TotalCents != 4500 {
		t.Fatalf("the response must be the full invoice: %+v", got1)
	}
	if !got1.UpdatedAt.After(got1.CreatedAt) {
		t.Errorf("updated_at should advance on finalize: %v vs %v", got1.UpdatedAt, got1.CreatedAt)
	}

	// The unfinalized draft was skipped, so it consumed no number.
	if rows := transitionRows(t, f, first.ID); len(rows) != 2 || rows[1] != "draft>open:finalized" {
		t.Fatalf("transitions = %v", rows)
	}
	// The stored copy matches what was returned.
	var stored invoiceJSON
	f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+first.ID, nil).JSON(t, &stored)
	if *stored.InvoiceSequenceNumber != *got1.InvoiceSequenceNumber {
		t.Fatal("GET must show the assigned number")
	}
}

func TestFinalizingTwiceIsRejected(t *testing.T) {
	f := setup(t)
	inv := create(t, f, f.a.Key, f.customer)
	if r := transition(t, f, f.a.Key, inv.ID, "finalize"); r.Code != 200 {
		t.Fatalf("first finalize: %d %s", r.Code, r.Body)
	}
	r := transition(t, f, f.a.Key, inv.ID, "finalize")
	if r.Code != http.StatusConflict || r.ErrorCode(t) != "invalid_transition" {
		t.Fatalf("second finalize: %d %s", r.Code, r.Body)
	}
	// ...and it must not have burned a number.
	next := create(t, f, f.a.Key, f.customer)
	var got invoiceJSON
	transition(t, f, f.a.Key, next.ID, "finalize").JSON(t, &got)
	if *got.InvoiceSequenceNumber != "INV-000002/26-27" {
		t.Fatalf("got %s, want INV-000002/26-27", *got.InvoiceSequenceNumber)
	}
}

func TestVoidKeepsTheNumberAndNeverReusesIt(t *testing.T) {
	f := setup(t)

	draft := create(t, f, f.a.Key, f.customer)
	var voidedDraft invoiceJSON
	transition(t, f, f.a.Key, draft.ID, "void").JSON(t, &voidedDraft)
	if voidedDraft.Status != "void" || voidedDraft.InvoiceSequenceNumber != nil {
		t.Fatalf("a voided draft never had a number: %+v", voidedDraft)
	}

	open := create(t, f, f.a.Key, f.customer)
	transition(t, f, f.a.Key, open.ID, "finalize")
	var voidedOpen invoiceJSON
	transition(t, f, f.a.Key, open.ID, "void").JSON(t, &voidedOpen)
	if voidedOpen.Status != "void" || voidedOpen.InvoiceSequenceNumber == nil || *voidedOpen.InvoiceSequenceNumber != "INV-000001/26-27" {
		t.Fatalf("a voided invoice keeps its number: %+v", voidedOpen)
	}

	next := create(t, f, f.a.Key, f.customer)
	var got invoiceJSON
	transition(t, f, f.a.Key, next.ID, "finalize").JSON(t, &got)
	if *got.InvoiceSequenceNumber != "INV-000002/26-27" {
		t.Fatalf("the voided invoice's number must not be reused, got %s", *got.InvoiceSequenceNumber)
	}
}

func TestUncollectibleCanStillBeVoided(t *testing.T) {
	f := setup(t)
	inv := create(t, f, f.a.Key, f.customer)
	for _, step := range []struct{ action, status string }{
		{"finalize", "open"}, {"mark_uncollectible", "uncollectible"}, {"void", "void"},
	} {
		var got invoiceJSON
		r := transition(t, f, f.a.Key, inv.ID, step.action)
		r.JSON(t, &got)
		if r.Code != 200 || got.Status != step.status {
			t.Fatalf("%s: %d %s", step.action, r.Code, r.Body)
		}
	}
	want := []string{"->draft:created", "draft>open:finalized", "open>uncollectible:marked_uncollectible", "uncollectible>void:voided"}
	got := transitionRows(t, f, inv.ID)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

func TestTransitionsAreTenantScoped(t *testing.T) {
	f := setup(t)
	inv := create(t, f, f.a.Key, f.customer)
	for _, action := range []string{"finalize", "void", "mark_uncollectible"} {
		for name, tt := range map[string]struct{ key, id string }{
			"another business's invoice": {f.b.Key, inv.ID},
			"nonexistent invoice":        {f.a.Key, uuid.NewString()},
			"malformed id":               {f.a.Key, "not-a-uuid"},
		} {
			r := transition(t, f, tt.key, tt.id, action)
			if r.Code != http.StatusNotFound || r.ErrorCode(t) != "invoice_not_found" {
				t.Errorf("%s %s: got %d %s", action, name, r.Code, r.Body)
			}
		}
		if r := transition(t, f, "", inv.ID, action); r.Code != http.StatusUnauthorized {
			t.Errorf("%s without a key: got %d", action, r.Code)
		}
	}
	assertStatus(t, f, inv.ID, StatusDraft)
	if n := count(t, f.env, "invoice_number_sequences"); n != 0 {
		t.Errorf("rejected requests created %d counters", n)
	}
}

func TestEachBusinessHasItsOwnCounter(t *testing.T) {
	f := setup(t)
	for _, tt := range []struct{ key, customer string }{{f.a.Key, f.customer}, {f.b.Key, f.other}} {
		inv := create(t, f, tt.key, tt.customer)
		var got invoiceJSON
		transition(t, f, tt.key, inv.ID, "finalize").JSON(t, &got)
		if *got.InvoiceSequenceNumber != "INV-000001/26-27" {
			t.Fatalf("every business starts at 1, got %s", *got.InvoiceSequenceNumber)
		}
	}
}

func TestCounterRestartsInTheNewFinancialYearInIndianTime(t *testing.T) {
	f := setup(t)
	finalizeAt := func(at time.Time) string {
		f.setClock(at)
		inv := create(t, f, f.a.Key, f.customer)
		var got invoiceJSON
		if r := transition(t, f, f.a.Key, inv.ID, "finalize"); r.Code != 200 {
			t.Fatalf("finalize: %d %s", r.Code, r.Body)
		}
		f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+inv.ID, nil).JSON(t, &got)
		return *got.InvoiceSequenceNumber
	}

	// 23:59:59 IST on 31 March is still 2026-27.
	if got := finalizeAt(time.Date(2027, 3, 31, 18, 29, 59, 0, time.UTC)); got != "INV-000001/26-27" {
		t.Fatalf("got %s", got)
	}
	if got := finalizeAt(time.Date(2027, 3, 31, 18, 29, 59, 0, time.UTC)); got != "INV-000002/26-27" {
		t.Fatalf("got %s", got)
	}
	// 00:00:00 IST on 1 April (still 31 March in UTC) is 2027-28 with a fresh counter.
	if got := finalizeAt(time.Date(2027, 3, 31, 18, 30, 0, 0, time.UTC)); got != "INV-000001/27-28" {
		t.Fatalf("got %s", got)
	}
	// The old year's counter is untouched.
	var last int64
	err := f.env.Pool.QueryRow(context.Background(),
		`SELECT last_value FROM invoice_number_sequences WHERE business_id = $1 AND fiscal_year = 2026`, f.a.BusinessID).Scan(&last)
	if err != nil || last != 2 {
		t.Fatalf("2026 counter = %d, %v", last, err)
	}
}

func TestConcurrentFinalizesIssueGaplessUniqueNumbers(t *testing.T) {
	f := setup(t)
	const n = 25
	ids := make([]string, n)
	for i := range ids {
		ids[i] = create(t, f, f.a.Key, f.customer).ID
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, n)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = transition(t, f, f.a.Key, id, "finalize").Code
		}()
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("finalize %d returned %d", i, c)
		}
	}
	rows, err := f.env.Pool.Query(context.Background(),
		`SELECT invoice_sequence_number FROM invoices WHERE business_id = $1 AND status = 'open'`, f.a.BusinessID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	sort.Strings(got)
	if len(got) != n {
		t.Fatalf("got %d numbered invoices, want %d", len(got), n)
	}
	for i, s := range got {
		if want := fmt.Sprintf("INV-%06d/26-27", i+1); s != want {
			t.Fatalf("position %d: %s, want %s (numbers must be consecutive with no gaps or duplicates)", i, s, want)
		}
	}
}

func TestConcurrentFinalizeOfOneInvoiceSucceedsOnce(t *testing.T) {
	f := setup(t)

	const n, rounds = 12, 8
	for round := 1; round <= rounds; round++ {
		inv := create(t, f, f.a.Key, f.customer)
		start := make(chan struct{}) // release everyone together so the requests really overlap
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[i] = transition(t, f, f.a.Key, inv.ID, "finalize").Code
			}()
		}
		close(start)
		wg.Wait()

		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != n-1 {
			t.Fatalf("round %d: codes = %v, want exactly one 200 and %d 409s", round, codes, n-1)
		}
		if rows := transitionRows(t, f, inv.ID); len(rows) != 2 {
			t.Fatalf("round %d: transitions = %v", round, rows)
		}
	}
	// One number per round, none burned by the losers.
	var last int64
	if err := f.env.Pool.QueryRow(context.Background(),
		`SELECT last_value FROM invoice_number_sequences WHERE business_id = $1`, f.a.BusinessID).Scan(&last); err != nil || last != rounds {
		t.Fatalf("counter = %d, want %d: %v", last, rounds, err)
	}
}

// Deterministic proof that a status change waits for the invoice row lock and
// then judges the state as it is after the lock is released.
func TestStatusChangeWaitsForTheRowLock(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inv := create(t, f, f.a.Key, f.customer)

	holder, err := f.env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, inv.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan testapi.Response, 1)
	go func() { done <- transition(t, f, f.a.Key, inv.ID, "finalize") }()

	select {
	case r := <-done:
		t.Fatalf("finalize completed (%d) while another transaction held the row lock", r.Code)
	case <-time.After(400 * time.Millisecond):
	}

	// The lock holder voids the invoice and commits; the waiting finalize must now see 'void'.
	if _, err := holder.Exec(ctx, `UPDATE invoices SET status = 'void' WHERE id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.Code != http.StatusConflict || r.ErrorCode(t) != "invalid_transition" || !strings.Contains(r.ErrorMessage(t), "void") {
		t.Fatalf("got %d %s, want 409 invalid_transition naming void", r.Code, r.Body)
	}
}

func TestConcurrentFinalizeAndVoidLeaveOneConsistentOutcome(t *testing.T) {
	f := setup(t)
	for round := 0; round < 10; round++ {
		inv := create(t, f, f.a.Key, f.customer)
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make(map[string]int)
		var mu sync.Mutex
		for _, action := range []string{"finalize", "void"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c := transition(t, f, f.a.Key, inv.ID, action).Code
				mu.Lock()
				codes[action] = c
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()

		var status Status
		if err := f.env.Pool.QueryRow(context.Background(), `SELECT status FROM invoices WHERE id = $1`, inv.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		rows := transitionRows(t, f, inv.ID)
		switch status {
		case StatusOpen: // finalize won; void must then have been refused or lost
			if codes["finalize"] != 200 {
				t.Fatalf("round %d: open but finalize returned %d", round, codes["finalize"])
			}
		case StatusVoid:
			if codes["void"] != 200 {
				t.Fatalf("round %d: void but void returned %d", round, codes["void"])
			}
		default:
			t.Fatalf("round %d: unexpected status %s", round, status)
		}
		// finalize then void is also legal (open -> void); the history must match the end state.
		if len(rows) < 2 || len(rows) > 3 || !strings.Contains(rows[len(rows)-1], ">"+string(status)+":") {
			t.Fatalf("round %d: status %s but history %v", round, status, rows)
		}
	}
}

// If anything after the counter increment fails, the number is not consumed.
func TestFailedFinalizeLeavesNoGap(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inv := create(t, f, f.a.Key, f.customer)
	if _, err := f.env.Pool.Exec(ctx, `
		CREATE FUNCTION fail_open() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN IF NEW.to_status = 'open' THEN RAISE EXCEPTION 'simulated failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_open BEFORE INSERT ON invoice_transitions
		FOR EACH ROW EXECUTE FUNCTION fail_open();`); err != nil {
		t.Fatal(err)
	}

	r := transition(t, f, f.a.Key, inv.ID, "finalize")
	if r.Code != http.StatusInternalServerError || strings.Contains(string(r.Body), "simulated failure") {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	assertStatus(t, f, inv.ID, StatusDraft)
	if n := count(t, f.env, "invoice_number_sequences"); n != 0 {
		t.Fatalf("the failed finalize left %d counter rows", n)
	}

	if _, err := f.env.Pool.Exec(ctx, `DROP TRIGGER fail_open ON invoice_transitions`); err != nil {
		t.Fatal(err)
	}
	var got invoiceJSON
	transition(t, f, f.a.Key, inv.ID, "finalize").JSON(t, &got)
	if got.InvoiceSequenceNumber == nil || *got.InvoiceSequenceNumber != "INV-000001/26-27" {
		t.Fatalf("the retry must get number 1, got %v", got.InvoiceSequenceNumber)
	}
}

func TestExhaustedCounterFailsTheFinalizeInsteadOfBreakingTheFormat(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.env.Pool.Exec(ctx,
		`INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 2026, 999998)`,
		f.a.BusinessID); err != nil {
		t.Fatal(err)
	}

	last := create(t, f, f.a.Key, f.customer)
	var got invoiceJSON
	transition(t, f, f.a.Key, last.ID, "finalize").JSON(t, &got)
	if got.InvoiceSequenceNumber == nil || *got.InvoiceSequenceNumber != "INV-999999/26-27" || len(*got.InvoiceSequenceNumber) != 16 {
		t.Fatalf("the last number must still fit in 16 characters: %v", got.InvoiceSequenceNumber)
	}

	overflow := create(t, f, f.a.Key, f.customer)
	r := transition(t, f, f.a.Key, overflow.ID, "finalize")
	if r.Code != http.StatusInternalServerError || r.ErrorCode(t) != "internal_error" {
		t.Fatalf("got %d %s", r.Code, r.Body)
	}
	assertStatus(t, f, overflow.ID, StatusDraft)
	var counter int64
	if err := f.env.Pool.QueryRow(ctx, `SELECT last_value FROM invoice_number_sequences WHERE business_id = $1`, f.a.BusinessID).Scan(&counter); err != nil || counter != 999999 {
		t.Fatalf("counter = %d, %v", counter, err)
	}
}

func TestNumberingConstraints(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a1 := create(t, f, f.a.Key, f.customer)
	a2 := create(t, f, f.a.Key, f.customer)
	b1 := create(t, f, f.b.Key, f.other)

	set := func(id, n string) error {
		_, err := f.env.Pool.Exec(ctx, `UPDATE invoices SET invoice_sequence_number = $2 WHERE id = $1`, id, n)
		return err
	}
	if err := set(a1.ID, "INV-000001/26-27"); err != nil {
		t.Fatal(err)
	}
	if err := set(b1.ID, "INV-000001/26-27"); err != nil {
		t.Fatalf("another business may use the same number: %v", err)
	}

	tests := []struct {
		name string
		err  error
		code string
	}{
		{"a number is unique within a business", set(a2.ID, "INV-000001/26-27"), "23505"},
		{"counter cannot exceed six digits", exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 2030, 1000000)`, f.a.BusinessID), "23514"},
		{"counter cannot be zero", exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 2031, 0)`, f.a.BusinessID), "23514"},
		{"prefix is at most three characters", exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, prefix, last_value) VALUES ($1, 2032, 'ABCD', 1)`, f.a.BusinessID), "23514"},
		{"prefix is alphanumeric", exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, prefix, last_value) VALUES ($1, 2033, 'A/B', 1)`, f.a.BusinessID), "23514"},
		{"fiscal year is a plausible year", exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 1999, 1)`, f.a.BusinessID), "23514"},
		{"one counter per business per year", func() error {
			_ = exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 2040, 1)`, f.a.BusinessID)
			return exec(f, `INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value) VALUES ($1, 2040, 1)`, f.a.BusinessID)
		}(), "23505"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pgErr *pgconn.PgError
			if !errors.As(tt.err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("got %v, want SQLSTATE %s", tt.err, tt.code)
			}
		})
	}
}

func exec(f fixture, sql string, args ...any) error {
	_, err := f.env.Pool.Exec(context.Background(), sql, args...)
	return err
}
