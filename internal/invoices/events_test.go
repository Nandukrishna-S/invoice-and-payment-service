package invoices

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/psp"
)

type deliveryJSON struct {
	ID         string
	EventID    string
	EndpointID string
	Type       string
	Status     string
	Payload    []byte
}

type eventBody struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		Object map[string]any `json:"object"`
	} `json:"data"`
}

func (b deliveryJSON) event(t testing.TB) eventBody {
	t.Helper()
	var e eventBody
	if err := json.Unmarshal(b.Payload, &e); err != nil {
		t.Fatalf("payload is not an event: %v: %s", err, b.Payload)
	}
	return e
}

func registerEndpoint(t testing.TB, f fixture, key, url string) string {
	t.Helper()
	r := f.env.Do(t, key, http.MethodPost, "/webhook_endpoints", map[string]string{"url": url})
	if r.Code != http.StatusCreated {
		t.Fatalf("register endpoint: %d %s", r.Code, r.Body)
	}
	var e struct {
		ID string `json:"id"`
	}
	r.JSON(t, &e)
	return e.ID
}

func deliveries(t testing.TB, f fixture, where string, args ...any) []deliveryJSON {
	t.Helper()
	q := `SELECT id::text, event_id, endpoint_id::text, type, status, payload::text FROM webhook_deliveries`
	if where != "" {
		q += " WHERE " + where
	}
	rows, err := f.env.Pool.Query(context.Background(), q+" ORDER BY id", args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deliveryJSON
	for rows.Next() {
		var d deliveryJSON
		var payload string
		if err := rows.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Type, &d.Status, &payload); err != nil {
			t.Fatal(err)
		}
		d.Payload = []byte(payload)
		out = append(out, d)
	}
	return out
}

func ofType(ds []deliveryJSON, typ string) []deliveryJSON {
	var out []deliveryJSON
	for _, d := range ds {
		if d.Type == typ {
			out = append(out, d)
		}
	}
	return out
}

func invoiceMap(t testing.TB, f fixture, id string) map[string]any {
	t.Helper()
	r := f.env.Do(t, f.a.Key, http.MethodGet, "/invoices/"+id, nil)
	var m map[string]any
	r.JSON(t, &m)
	return m
}

// two endpoints for business A, one for business B
func withEndpoints(t *testing.T) (fixture, []string, string) {
	f := setupPay(t, time.Hour, 300*time.Millisecond)
	a := []string{registerEndpoint(t, f, f.a.Key, "https://a.example/1"), registerEndpoint(t, f, f.a.Key, "https://a.example/2")}
	b := registerEndpoint(t, f, f.b.Key, "https://b.example/1")
	return f, a, b
}

func TestInvoiceCreatedIsQueuedForEveryActiveEndpointOfTheBusiness(t *testing.T) {
	f, mine, theirs := withEndpoints(t)
	inv := create(t, f, f.a.Key, f.customer)

	ds := deliveries(t, f, "")
	if len(ds) != 2 {
		t.Fatalf("got %d rows, want one per endpoint of business A", len(ds))
	}
	seen := map[string]bool{}
	for _, d := range ds {
		seen[d.EndpointID] = true
		if d.Type != "invoice.created" || d.Status != "pending" || d.EventID != ds[0].EventID {
			t.Fatalf("unexpected row: %+v", d)
		}
	}
	if !seen[mine[0]] || !seen[mine[1]] || seen[theirs] {
		t.Fatalf("endpoints reached: %v (B's endpoint %s must not be among them)", seen, theirs)
	}

	e := ds[0].event(t)
	if e.ID != ds[0].EventID || !strings.HasPrefix(e.ID, "evt_") || e.Type != "invoice.created" || e.CreatedAt.IsZero() {
		t.Fatalf("event header: %+v", e)
	}
	// data.object is the invoice exactly as GET /invoices/{id} shows it.
	if got := invoiceMap(t, f, inv.ID); !reflect.DeepEqual(e.Data.Object, got) {
		t.Fatalf("snapshot differs from GET:\n event: %v\n   GET: %v", e.Data.Object, got)
	}
}

func TestEventPayloadMatchesTheWebhookEventSchema(t *testing.T) {
	f, _, _ := withEndpoints(t)
	inv := openInvoice(t, f)
	pay(t, f, inv.ID, "tok_success")
	pay(t, f, openInvoice(t, f).ID, "tok_card_declined")

	required := []string{"id", "customer_id", "status", "currency", "total_cents", "due_date", "line_items", "created_at", "updated_at"}
	for _, d := range deliveries(t, f, "") {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(d.Payload, &raw); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"id", "type", "created_at", "data"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("%s: event lacks %q", d.Type, k)
			}
		}
		if len(raw) != 4 {
			t.Errorf("%s: event has extra top-level fields: %v", d.Type, keys(raw))
		}
		e := d.event(t)
		if e.CreatedAt.Location() != time.UTC {
			t.Errorf("%s: created_at should be UTC: %v", d.Type, e.CreatedAt)
		}
		for _, k := range required {
			if _, ok := e.Data.Object[k]; !ok {
				t.Errorf("%s: invoice snapshot lacks %q", d.Type, k)
			}
		}
		for _, banned := range []string{"business_id", "latest_payment_attempt_id", "email"} {
			if _, ok := e.Data.Object[banned]; ok {
				t.Errorf("%s: snapshot must not contain %q", d.Type, banned)
			}
		}
		if strings.Contains(string(d.Payload), "acme.test") {
			t.Errorf("%s: a customer email leaked into the payload", d.Type)
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNoEventForTransitionsThatAreNotInTheContract(t *testing.T) {
	f, _, _ := withEndpoints(t)
	inv := create(t, f, f.a.Key, f.customer)
	before := len(deliveries(t, f, ""))
	for _, action := range []string{"finalize", "mark_uncollectible", "void"} {
		if r := transition(t, f, f.a.Key, inv.ID, action); r.Code != 200 {
			t.Fatalf("%s: %d %s", action, r.Code, r.Body)
		}
	}
	if after := len(deliveries(t, f, "")); after != before {
		t.Fatalf("finalize, mark_uncollectible and void must not queue events (%d -> %d rows)", before, after)
	}
}

func TestNoEventsAreWrittenWhenNobodyIsListening(t *testing.T) {
	f := setupPay(t, time.Hour, 300*time.Millisecond)
	registerEndpoint(t, f, f.b.Key, "https://b.example/1") // only the other business
	inv := openInvoice(t, f)
	pay(t, f, inv.ID, "tok_success")
	if n := len(deliveries(t, f, "")); n != 0 {
		t.Fatalf("%d rows written for a business with no endpoints", n)
	}
}

func TestPaidEventIsQueuedOncePerEndpointWithThePaidInvoice(t *testing.T) {
	f, mine, _ := withEndpoints(t)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_success"))
	if a.Status != "succeeded" {
		t.Fatalf("%+v", a)
	}

	paid := ofType(deliveries(t, f, ""), "invoice.paid")
	if len(paid) != len(mine) {
		t.Fatalf("got %d paid rows, want one per endpoint (%d)", len(paid), len(mine))
	}
	if paid[0].EventID != paid[1].EventID {
		t.Fatal("both endpoints must get the same event id")
	}
	e := paid[0].event(t)
	if e.Data.Object["status"] != "paid" || e.Data.Object["paid_at"] == nil || e.Data.Object["id"] != inv.ID {
		t.Fatalf("snapshot should be the paid invoice: %v", e.Data.Object)
	}
	if got := invoiceMap(t, f, inv.ID); !reflect.DeepEqual(e.Data.Object, got) {
		t.Fatalf("snapshot differs from GET after payment:\n event: %v\n   GET: %v", e.Data.Object, got)
	}
	if n := len(ofType(deliveries(t, f, ""), "invoice.payment_failed")); n != 0 {
		t.Fatalf("a success must not queue a failure event (%d)", n)
	}
}

func TestPaymentFailedEventLeavesTheInvoiceAsItWas(t *testing.T) {
	f, mine, _ := withEndpoints(t)
	inv := openInvoice(t, f)
	pay(t, f, inv.ID, "tok_insufficient_funds")
	pay(t, f, inv.ID, "tok_card_declined") // a second attempt is a second event

	failed := ofType(deliveries(t, f, ""), "invoice.payment_failed")
	if len(failed) != 2*len(mine) {
		t.Fatalf("got %d rows, want 2 events x %d endpoints", len(failed), len(mine))
	}
	ids := map[string]int{}
	for _, d := range failed {
		ids[d.EventID]++
		obj := d.event(t).Data.Object
		if obj["status"] != "open" || obj["paid_at"] != nil {
			t.Fatalf("the invoice is unchanged by a decline: %v", obj)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("two declines must be two distinct events, got ids %v", ids)
	}
	if n := len(ofType(deliveries(t, f, ""), "invoice.paid")); n != 0 {
		t.Fatalf("a decline must not queue a paid event (%d)", n)
	}
}

func TestAnUnknownOutcomeQueuesNothingUntilItIsResolved(t *testing.T) {
	f, mine, _ := withEndpoints(t)
	inv := openInvoice(t, f)
	a := attemptOf(t, pay(t, f, inv.ID, "tok_network_error"))
	if a.Status != "pending" {
		t.Fatalf("%+v", a)
	}
	if n := len(ofType(deliveries(t, f, ""), "invoice.paid")) + len(ofType(deliveries(t, f, ""), "invoice.payment_failed")); n != 0 {
		t.Fatalf("%d outcome events while the outcome is unknown", n)
	}
	reconcile(t, f, a)
	if n := len(ofType(deliveries(t, f, ""), "invoice.paid")); n != len(mine) {
		t.Fatalf("after reconciliation: %d paid rows, want %d", n, len(mine))
	}
}

func TestReplaysAndRacingResolversQueueEachEventOnce(t *testing.T) {
	f, mine, _ := withEndpoints(t)
	inv := openInvoice(t, f)
	key := uniqueKey()
	payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500)
	for i := 0; i < 3; i++ {
		payWith(t, f, f.a.Key, inv.ID, key, "tok_success", 4500) // replays
	}
	if n := len(ofType(deliveries(t, f, ""), "invoice.paid")); n != len(mine) {
		t.Fatalf("replays queued extra events: %d rows, want %d", n, len(mine))
	}

	// Eight resolvers racing on one pending payment: still exactly one event.
	g, mineG, _ := withEndpoints(t)
	pending := openInvoice(t, g)
	a := attemptOf(t, pay(t, g, pending.ID, "tok_network_error"))
	ref := g.paymentRef(t, a.ID)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := g.svc.ResolvePayment(context.Background(), ref, psp.Outcome{Status: psp.Succeeded, PSPRef: "p"}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := len(ofType(deliveries(t, g, ""), "invoice.paid")); n != len(mineG) {
		t.Fatalf("racing resolvers queued %d paid rows, want exactly %d", n, len(mineG))
	}
}

func TestEndpointsRegisteredLaterMissPastEventsAndDisabledOnesMissAll(t *testing.T) {
	f := setupPay(t, time.Hour, 300*time.Millisecond)
	early := registerEndpoint(t, f, f.a.Key, "https://a.example/early")
	disabled := registerEndpoint(t, f, f.a.Key, "https://a.example/off")
	if _, err := f.env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	inv := create(t, f, f.a.Key, f.customer)
	late := registerEndpoint(t, f, f.a.Key, "https://a.example/late")
	transition(t, f, f.a.Key, inv.ID, "finalize")
	pay(t, f, inv.ID, "tok_success")

	per := map[string][]string{}
	for _, d := range deliveries(t, f, "") {
		per[d.EndpointID] = append(per[d.EndpointID], d.Type)
	}
	if !reflect.DeepEqual(per[early], []string{"invoice.created", "invoice.paid"}) || !reflect.DeepEqual(per[late], []string{"invoice.paid"}) || len(per[disabled]) != 0 {
		t.Fatalf("early=%v late=%v disabled=%v", per[early], per[late], per[disabled])
	}
}

// ---- same-transaction guarantees ------------------------------------------------

func failOnDeliveryInsert(t testing.TB, f fixture, deferred bool) {
	t.Helper()
	sql := `
		CREATE FUNCTION refuse_delivery() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'simulated outbox failure'; END $$;`
	if deferred {
		// fires at COMMIT, after every statement of the transaction has succeeded
		sql += `CREATE CONSTRAINT TRIGGER refuse_delivery AFTER INSERT ON webhook_deliveries
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_delivery();`
	} else {
		sql += `CREATE TRIGGER refuse_delivery BEFORE INSERT ON webhook_deliveries
			FOR EACH ROW EXECUTE FUNCTION refuse_delivery();`
	}
	if _, err := f.env.Pool.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func allowDeliveryInsert(t testing.TB, f fixture) {
	t.Helper()
	if _, err := f.env.Pool.Exec(context.Background(), `DROP TRIGGER refuse_delivery ON webhook_deliveries`); err != nil {
		t.Fatal(err)
	}
}

// If queueing the event fails, the change it describes must not be committed,
// whether the failure is immediate or only discovered at COMMIT.
func TestInvoiceCreatedAndItsEventCommitOrRollBackTogether(t *testing.T) {
	for name, deferred := range map[string]bool{"fails when queued": false, "fails at commit": true} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := withEndpoints(t)
			failOnDeliveryInsert(t, f, deferred)
			logs := captureLogs(t)

			r := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4500))
			if r.Code != http.StatusInternalServerError || r.ErrorCode(t) != "internal_error" || strings.Contains(string(r.Body), "simulated") {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}
			if !strings.Contains(logs.String(), "simulated outbox failure") {
				t.Fatal("the cause must be in the logs")
			}
			for _, table := range []string{"invoices", "invoice_line_items", "invoice_transitions", "webhook_deliveries"} {
				if n := count(t, f.env, table); n != 0 {
					t.Errorf("%s has %d rows after a failed create; the invoice and its event must roll back together", table, n)
				}
			}

			allowDeliveryInsert(t, f)
			if r := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4500)); r.Code != http.StatusCreated {
				t.Fatalf("after the fault is gone: %d %s", r.Code, r.Body)
			}
			if n := len(deliveries(t, f, "")); n != 2 {
				t.Fatalf("%d rows, want 2", n)
			}
		})
	}
}

func TestPaymentOutcomeAndItsEventCommitOrRollBackTogether(t *testing.T) {
	cases := []struct {
		name      string
		token     string
		eventType string
		wantAtt   string
		wantInv   string
	}{
		{"paid", "tok_success", "invoice.paid", "succeeded", "paid"},
		{"payment_failed", "tok_card_declined", "invoice.payment_failed", "failed", "open"},
	}
	for _, c := range cases {
		for name, deferred := range map[string]bool{"immediate": false, "at commit": true} {
			t.Run(c.name+" "+name, func(t *testing.T) {
				f, mine, _ := withEndpoints(t)
				inv := openInvoice(t, f)
				failOnDeliveryInsert(t, f, deferred)

				r := pay(t, f, inv.ID, c.token)
				a := attemptOf(t, r)
				if r.Code != http.StatusAccepted || a.Status != "pending" {
					t.Fatalf("a resolution that cannot be recorded must leave the attempt pending: %d %s", r.Code, r.Body)
				}
				if got := getInvoice(t, f, inv.ID); got.Status != "open" || got.PaidAt != nil {
					t.Fatalf("invoice changed although its event could not be queued: %+v", got)
				}
				if n := countWhere(t, f, "payments", "status = 'pending'"); n != 1 {
					t.Fatal("the payment must still be pending")
				}
				if n := len(ofType(deliveries(t, f, ""), c.eventType)); n != 0 {
					t.Fatalf("%d %s rows were left behind", n, c.eventType)
				}
				if rows := transitionRows(t, f, inv.ID); len(rows) != 2 {
					t.Fatalf("no transition may be written either: %v", rows)
				}

				// Once queueing works again, the reconciler finishes the job, and only then does the event exist.
				allowDeliveryInsert(t, f)
				reconcile(t, f, a)
				if got := attemptOf(t, f.env.Do(t, f.a.Key, http.MethodGet, "/payment_attempts/"+a.ID, nil)); got.Status != c.wantAtt {
					t.Fatalf("attempt after recovery: %s", got.Status)
				}
				if got := getInvoice(t, f, inv.ID); got.Status != c.wantInv {
					t.Fatalf("invoice after recovery: %s", got.Status)
				}
				if n := len(ofType(deliveries(t, f, ""), c.eventType)); n != len(mine) {
					t.Fatalf("after recovery: %d %s rows, want %d", n, c.eventType, len(mine))
				}
			})
		}
	}
}

// The other direction: the event rows already exist inside the transaction when the change
// is refused at COMMIT (a deferred constraint on the transition table fires last), so the
// queued event must vanish with it.
func TestAnEventIsNeverLeftBehindByAChangeThatRolledBack(t *testing.T) {
	refuseAtCommit := func(t *testing.T, f fixture, status string) {
		t.Helper()
		if _, err := f.env.Pool.Exec(context.Background(), `
			CREATE FUNCTION refuse_transition() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN IF NEW.to_status = '`+status+`' THEN RAISE EXCEPTION 'simulated commit-time failure'; END IF; RETURN NULL; END $$;
			CREATE CONSTRAINT TRIGGER refuse_transition AFTER INSERT ON invoice_transitions
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_transition();`); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("create", func(t *testing.T) {
		f, _, _ := withEndpoints(t)
		refuseAtCommit(t, f, "draft")
		r := f.env.Do(t, f.a.Key, http.MethodPost, "/invoices", body(f.customer, 4500))
		if r.Code != http.StatusInternalServerError {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if n := len(deliveries(t, f, "")); n != 0 || count(t, f.env, "invoices") != 0 {
			t.Fatalf("%d event rows and %d invoices survived a transaction that failed at commit", n, count(t, f.env, "invoices"))
		}
	})

	t.Run("paid", func(t *testing.T) {
		f, _, _ := withEndpoints(t)
		inv := openInvoice(t, f)
		refuseAtCommit(t, f, "paid")
		a := attemptOf(t, pay(t, f, inv.ID, "tok_success"))
		if a.Status != "pending" {
			t.Fatalf("%+v", a)
		}
		if n := len(ofType(deliveries(t, f, ""), "invoice.paid")); n != 0 {
			t.Fatalf("%d paid events exist for a payment that did not commit", n)
		}
		if getInvoice(t, f, inv.ID).Status != "open" || countWhere(t, f, "payments", "status = 'pending'") != 1 {
			t.Fatal("nothing else may have committed either")
		}
	})
}

func TestEventsNeverCarryEndpointSecretsAndLogsStayClean(t *testing.T) {
	logs := captureLogs(t)
	f, _, _ := withEndpoints(t)
	pay(t, f, openInvoice(t, f).ID, "tok_success")
	var secrets []string
	rows, err := f.env.Pool.Query(context.Background(), `SELECT secret FROM webhook_endpoints`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, s)
	}
	rows.Close()
	if len(secrets) != 3 {
		t.Fatalf("expected 3 secrets, got %d", len(secrets))
	}
	for _, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Fatal("an endpoint secret reached the logs")
		}
		for _, d := range deliveries(t, f, "") {
			if strings.Contains(string(d.Payload), s) {
				t.Fatal("an endpoint secret is inside an event body")
			}
		}
	}
	_ = uuid.Nil
}
