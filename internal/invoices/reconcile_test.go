package invoices

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/reconciler"
	"invoice-and-payment-service/internal/testpsp"
)

func attemptStatus(t testing.TB, f fixture, id string) string {
	t.Helper()
	return attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+id, nil)).Status
}

// ---- the real reconciler, the real pay flow and the real provider ------------------

// tok_timeout: the provider keeps working long after the pay call gives up. The reconciler
// must see "processing" without changing anything, then record the success once it lands.
func TestReconcilerSettlesATimedOutPayment(t *testing.T) {
	f := setupPay(t, 700*time.Millisecond, 200*time.Millisecond)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_timeout"))
	if a.Status != "pending" {
		t.Fatalf("the pay call should have left it pending: %+v", a)
	}
	rec := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0)

	res, err := rec.RunOnce(context.Background())
	if err != nil || res.Processing != 1 || res.Resolved != 0 {
		t.Fatalf("while the provider is still working: %+v %v", res, err)
	}
	if attemptStatus(t, f, a.ID) != "pending" || getInvoice(t, f, inv.ID).Status != "open" {
		t.Fatal("asking the provider must not change anything while it is processing")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err = rec.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Resolved == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never resolved: %+v", res)
		}
		time.Sleep(40 * time.Millisecond)
	}

	got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil))
	if got.Status != "succeeded" || got.PSPRefID == nil || got.ResolvedAt == nil {
		t.Fatalf("attempt: %+v", got)
	}
	if inv := getInvoice(t, f, inv.ID); inv.Status != "paid" || inv.PaidAt == nil {
		t.Fatalf("invoice: %+v", inv)
	}
	if countWhere(t, f, "payments", "true") != 1 || countWhere(t, f, "invoice_payment_attempts", "true") != 1 || f.mock.Charges(t) != 1 {
		t.Fatal("exactly one payment, one attempt and one charge expected")
	}
	if rows := transitionRows(t, f, inv.ID); len(rows) != 3 || rows[2] != "open>paid:payment_succeeded" {
		t.Fatalf("transitions = %v", rows)
	}
	// Nothing left to do: a later sweep does not even ask the provider.
	if res, _ := rec.RunOnce(context.Background()); res.Checked != 0 {
		t.Fatalf("a resolved payment was checked again: %+v", res)
	}
}

// tok_network_error: the provider charged, then hung up. We never saw the answer.
func TestReconcilerSettlesADroppedConnection(t *testing.T) {
	f := setupPay(t, time.Hour, time.Second)
	pay(t, f, openInvoice(t, f).ID, "tok_success") // warm the connection pool
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error"))
	if a.Status != "pending" {
		t.Fatalf("%+v", a)
	}

	res, err := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0).RunOnce(context.Background())
	if err != nil || res.Resolved != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if inv := getInvoice(t, f, inv.ID); inv.Status != "paid" {
		t.Fatalf("invoice: %+v", inv)
	}
	if f.mock.Charges(t) != 2 { // the warm-up and this one: never charged twice
		t.Fatalf("charges = %d, want 2", f.mock.Charges(t))
	}
}

// lostAnswer charges at the real provider and then pretends the answer was lost,
// so the pay call sees an unknown outcome although the customer was charged.
type lostAnswer struct{ real *psp.Client }

func (l lostAnswer) Charge(ctx context.Context, ref uuid.UUID, token string, amount int64) psp.Outcome {
	l.real.Charge(ctx, ref, token, amount)
	return psp.Outcome{Status: psp.Unknown, Cause: context.DeadlineExceeded}
}

// neverSent loses the request before it reaches the provider (a crash between reserving and charging).
type neverSent struct{}

func (neverSent) Charge(context.Context, uuid.UUID, string, int64) psp.Outcome {
	return psp.Outcome{Status: psp.Unknown, Cause: context.Canceled}
}

func setupWithProvider(t *testing.T, wrap func(real *psp.Client) PSPClient) fixture {
	t.Helper()
	mock := testpsp.Start(t, time.Hour)
	real := psp.NewClient(mock.URL, time.Second, 2*time.Second)
	env, now, svc := newEnv(t, wrap(real))
	return fixture{env: env, now: now, mock: mock, svc: svc, a: env.NewTenant(t), b: env.NewTenant(t),
		queryClient: psp.NewClient(mock.URL, time.Second, 2*time.Second)}
}

func (f *fixture) init(t *testing.T) {
	f.customer = newCustomer(t, f.env, f.a.Key)
	f.other = newCustomer(t, f.env, f.b.Key)
}

// A decline whose answer we missed is recorded as a decline: the invoice stays open and can be retried.
func TestReconcilerRecordsADeclineTheInlineCallMissed(t *testing.T) {
	f := setupWithProvider(t, func(real *psp.Client) PSPClient { return lostAnswer{real} })
	f.init(t)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_insufficient_funds"))
	if a.Status != "pending" {
		t.Fatalf("%+v", a)
	}

	res, err := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0).RunOnce(context.Background())
	if err != nil || res.Resolved != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil))
	if got.Status != "failed" || got.FailureCode == nil || *got.FailureCode != "insufficient_funds" || got.PSPRefID != nil {
		t.Fatalf("attempt: %+v", got)
	}
	if inv := getInvoice(t, f, inv.ID); inv.Status != "open" || inv.PaidAt != nil {
		t.Fatalf("a decline leaves the invoice open: %+v", inv)
	}
	// With the attempt resolved, the invoice is payable again (a new key, a new attempt).
	if r := payWith(t, f, f.a.Key, inv.ID, uniqueKey(), "tok_insufficient_funds", 4500); r.Code != http.StatusAccepted {
		t.Fatalf("retry after reconciliation: %d %s", r.Code, r.Body)
	}
}

// The provider has no record of the payment (the request never left): the reconciler cannot
// know it failed, so it never says so. It keeps asking, keeps saying so in the logs, and resolves
// normally as soon as the provider does know it.
func TestReconcilerNeverFailsAPaymentTheProviderHasNoRecordOf(t *testing.T) {
	f := setupWithProvider(t, func(*psp.Client) PSPClient { return neverSent{} })
	f.init(t)
	logs := captureLogs(t)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_success"))
	ref := f.paymentRef(t, a.ID)
	rec := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0)

	for sweep := 1; sweep <= 3; sweep++ {
		res, err := rec.RunOnce(context.Background())
		if err != nil || res.NotFound != 1 || res.Resolved != 0 {
			t.Fatalf("sweep %d: %+v %v", sweep, res, err)
		}
	}
	if n := strings.Count(logs.String(), "provider has no record of a pending payment"); n != 3 {
		t.Fatalf("%d warnings for 3 sweeps: %s", n, logs.String())
	}
	if attemptStatus(t, f, a.ID) != "pending" || getInvoice(t, f, inv.ID).Status != "open" {
		t.Fatal("silence from the provider must never become a failure")
	}
	// While it is unresolved the invoice cannot be paid again or voided: it is safe, if blocked.
	if r := pay(t, f, inv.ID, "tok_success"); r.Code != http.StatusConflict || r.ErrorCode(t) != "payment_in_progress" {
		t.Fatalf("pay: %d %s", r.Code, r.Body)
	}

	// The charge reaches the provider after all (say a delayed message): the next sweep settles it.
	if out := f.queryClient.Charge(context.Background(), ref, "tok_success", 4500); out.Status != psp.Succeeded {
		t.Fatalf("%+v", out)
	}
	if res, _ := rec.RunOnce(context.Background()); res.Resolved != 1 {
		t.Fatalf("%+v", res)
	}
	if getInvoice(t, f, inv.ID).Status != "paid" {
		t.Fatal("the invoice should be paid once the provider knows the charge")
	}
}

// The checkpoint's failure test: whatever the provider does wrong, nothing is guessed.
func TestProviderOutagesNeverResolveOrFailAnything(t *testing.T) {
	for name, fault := range map[string]testpsp.Fault{
		"503 unavailable":    testpsp.FaultUnavailable,
		"connection dropped": testpsp.FaultDropped,
		"never answers":      testpsp.FaultHang,
	} {
		t.Run(name, func(t *testing.T) {
			f := setupPay(t, time.Hour, 300*time.Millisecond)
			pay(t, f, openInvoice(t, f).ID, "tok_success") // warm
			inv := openInvoice(t, f)
			a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error")) // charged, answer lost
			ref := f.paymentRef(t, a.ID)
			_ = ref
			rec := reconciler.New(f.env.Pool, psp.NewClient(f.mock.URL, 200*time.Millisecond, 300*time.Millisecond), f.svc, 0)

			f.mock.SetFault(fault)
			for sweep := 0; sweep < 3; sweep++ {
				start := time.Now()
				res, err := rec.RunOnce(context.Background())
				if err != nil || res.Unknown != 1 || res.Resolved != 0 || res.NotFound != 0 {
					t.Fatalf("sweep %d during the outage: %+v %v", sweep, res, err)
				}
				if time.Since(start) > 2*time.Second {
					t.Fatalf("a sweep took %v; the client timeout must bound it", time.Since(start))
				}
			}
			if attemptStatus(t, f, a.ID) != "pending" || getInvoice(t, f, inv.ID).Status != "open" || countWhere(t, f, "payments", "status = 'failed'") != 0 {
				t.Fatal("an outage must leave the payment pending, never failed")
			}
			// New payments during the outage are accepted as unknown, not failed either.
			during := openInvoice(t, f)
			if r := pay(t, f, during.ID, "tok_success"); r.Code != http.StatusAccepted || attemptOf(t, r).Status != "pending" {
				t.Fatalf("pay during the outage: %d %s", r.Code, r.Body)
			}

			f.mock.SetFault(testpsp.FaultNone)
			res, err := rec.RunOnce(context.Background())
			if err != nil || res.Resolved != 1 {
				t.Fatalf("after recovery: %+v %v", res, err)
			}
			if getInvoice(t, f, inv.ID).Status != "paid" {
				t.Fatal("the charge that happened must be recorded once the provider answers")
			}
			// The payment made during the outage never reached the provider: still pending, still not failed.
			if getInvoice(t, f, during.ID).Status != "open" || countWhere(t, f, "payments", "status = 'failed'") != 0 {
				t.Fatal("a payment the provider never saw must not be failed")
			}
		})
	}
}

func TestReconcilerRacingTheInlineResolutionAppliesOnce(t *testing.T) {
	f := setupPay(t, time.Hour, 200*time.Millisecond)
	for round := 0; round < 6; round++ {
		inv := openInvoice(t, f)
		a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error")) // charged at the provider, unresolved here
		ref := f.paymentRef(t, a.ID)
		f.queryClient.Query(context.Background(), ref) // make sure the provider has it

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0).RunOnce(context.Background()); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Add(1)
		go func() { // the "inline" path arriving at the same moment
			defer wg.Done()
			<-start
			if err := f.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "inline"}); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()

		if rows := transitionRows(t, f, inv.ID); len(rows) != 3 {
			t.Fatalf("round %d: transitions = %v, want exactly one paid transition", round, rows)
		}
		if getInvoice(t, f, inv.ID).Status != "paid" || countWhere(t, f, "invoice_payment_attempts", "invoice_id = $1", inv.ID) != 1 {
			t.Fatalf("round %d: inconsistent end state", round)
		}
	}
}

// The background loop, as main runs it: payments resolve with no manual sweep.
func TestReconcilerRunSettlesPaymentsOnItsOwn(t *testing.T) {
	f := setupPay(t, time.Hour, time.Second)
	pay(t, f, openInvoice(t, f).ID, "tok_success") // warm
	inv := openInvoice(t, f)
	pay(t, f, inv.ID, "tok_network_error")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reconciler.New(f.env.Pool, f.queryClient, f.svc, 0).Run(ctx, 50*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for getInvoice(t, f, inv.ID).Status != "paid" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the background reconciler did not pay the invoice")
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the reconciler did not stop on shutdown")
	}
}

// The minimum age keeps the reconciler away from a payment whose inline call may still be running.
func TestReconcilerLeavesInFlightPaymentsToTheInlineCall(t *testing.T) {
	f := setupPay(t, time.Hour, time.Second)
	pay(t, f, openInvoice(t, f).ID, "tok_success") // warm
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error"))

	res, err := reconciler.New(f.env.Pool, f.queryClient, f.svc, time.Hour).RunOnce(context.Background())
	if err != nil || res.Checked != 0 {
		t.Fatalf("a brand-new pending payment must be left alone: %+v %v", res, err)
	}
	if attemptStatus(t, f, a.ID) != "pending" {
		t.Fatal("untouched")
	}
	res, _ = reconciler.New(f.env.Pool, f.queryClient, f.svc, 0).RunOnce(context.Background())
	if res.Resolved != 1 {
		t.Fatalf("%+v", res)
	}
}

// If recording a definitive answer fails, nothing is half-written and the next sweep finishes the job.
func TestReconcilerRetriesAResolutionThatFailed(t *testing.T) {
	f := setupPay(t, time.Hour, time.Second)
	pay(t, f, openInvoice(t, f).ID, "tok_success") // warm
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error"))
	ctx := context.Background()
	if _, err := f.env.Pool.Exec(ctx, `
		CREATE FUNCTION fail_paid() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN IF NEW.to_status = 'paid' THEN RAISE EXCEPTION 'simulated failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_paid BEFORE INSERT ON invoice_transitions
		FOR EACH ROW EXECUTE FUNCTION fail_paid();`); err != nil {
		t.Fatal(err)
	}
	rec := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0)

	res, _ := rec.RunOnce(ctx)
	if res.Errors != 1 || res.Resolved != 0 {
		t.Fatalf("%+v", res)
	}
	if attemptStatus(t, f, a.ID) != "pending" || countWhere(t, f, "payments", "payment_ref_id = $1 AND status = 'pending'", f.paymentRef(t, a.ID)) != 1 {
		t.Fatal("a failed resolution must roll back completely")
	}

	if _, err := f.env.Pool.Exec(ctx, `DROP TRIGGER fail_paid ON invoice_transitions`); err != nil {
		t.Fatal(err)
	}
	if res, _ := rec.RunOnce(ctx); res.Resolved != 1 || getInvoice(t, f, inv.ID).Status != "paid" {
		t.Fatalf("%+v", res)
	}
}

func TestReconcilerNeverLogsACardToken(t *testing.T) {
	logs := captureLogs(t)
	f := setupPay(t, 500*time.Millisecond, 200*time.Millisecond)
	for _, token := range []string{"tok_timeout", "tok_network_error", "tok_card_declined"} {
		pay(t, f, openInvoice(t, f).ID, token)
	}
	f.mock.SetFault(testpsp.FaultUnavailable)
	rec := reconciler.New(f.env.Pool, f.queryClient, f.svc, 0)
	_, _ = rec.RunOnce(context.Background())
	f.mock.SetFault(testpsp.FaultNone)
	time.Sleep(600 * time.Millisecond)
	_, _ = rec.RunOnce(context.Background())
	if strings.Contains(logs.String(), "tok_") {
		t.Fatalf("a card token reached the logs: %s", logs.String())
	}
}
