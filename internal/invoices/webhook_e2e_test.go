package invoices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"invoice-and-payment-service/internal/webhookreceiver"
	"invoice-and-payment-service/internal/webhooks"
)

type e2e struct {
	f        fixture
	receiver *httptest.Server
	secret   string
	d        *webhooks.Dispatcher
}

// startE2E wires the whole path: real API flows, real outbox, real delivery worker, real receiver.
func startE2E(t *testing.T, path string, teachReceiver bool) e2e {
	t.Helper()
	f := setupPay(t, time.Hour, 300*time.Millisecond)
	recv := httptest.NewServer(webhookreceiver.New().Routes())
	t.Cleanup(recv.Close)

	r := f.env.Do(t, f.a.Key, http.MethodPost, "/webhook_endpoints", map[string]string{"url": recv.URL + path})
	var ep struct {
		Secret string `json:"secret"`
	}
	r.JSON(t, &ep)
	if r.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	x := e2e{f: f, receiver: recv, secret: ep.Secret, d: webhooks.NewDispatcher(f.env.Pool, []time.Duration{0, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond}, 2*time.Second)}
	if teachReceiver {
		x.teach(t, ep.Secret)
	}
	return x
}

func (x e2e) teach(t *testing.T, secret string) {
	t.Helper()
	resp, err := http.Post(x.receiver.URL+"/secrets", "application/json", strings.NewReader(`{"secret":"`+secret+`"}`))
	if err != nil || resp.StatusCode >= 300 {
		t.Fatalf("teach the receiver the secret: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func (x e2e) statusCounts(t *testing.T) map[string]int {
	t.Helper()
	rows, err := x.f.env.Pool.Query(context.Background(), `SELECT status, count(*) FROM webhook_deliveries GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func TestEveryEventReachesTheReceiverSignedAndVerified(t *testing.T) {
	logs := captureLogs(t)
	x := startE2E(t, "/hook", true)
	f := x.f

	paidInv := openInvoice(t, f)                   // created + (finalize: no event)
	pay(t, f, paidInv.ID, "tok_success")           // invoice.paid
	declinedInv := openInvoice(t, f)               // created
	pay(t, f, declinedInv.ID, "tok_card_declined") // invoice.payment_failed

	if n, err := x.d.Drain(context.Background()); err != nil || n != 4 {
		t.Fatalf("delivered %d (err %v), want 4 events", n, err)
	}
	if c := x.statusCounts(t); c["delivered"] != 4 || len(c) != 1 {
		t.Fatalf("delivery states: %v", c)
	}

	out := logs.String()
	if got := strings.Count(out, "webhook received, signature verified"); got != 4 {
		t.Fatalf("receiver verified %d deliveries, want 4:\n%s", got, out)
	}
	if strings.Contains(out, "NOT verified") {
		t.Fatalf("a delivery failed verification:\n%s", out)
	}
	for _, want := range []string{`"event_type":"invoice.created"`, `"event_type":"invoice.paid"`, `"event_type":"invoice.payment_failed"`,
		`"invoice_id":"` + paidInv.ID + `"`, `"invoice_status":"paid"`} {
		if !strings.Contains(out, want) {
			t.Errorf("receiver log lacks %s", want)
		}
	}
	for _, banned := range []string{x.secret, "tok_", "acme.test"} {
		if strings.Contains(out, banned) {
			t.Errorf("logs contain %q", banned)
		}
	}

	// Delivery is done once: another pass finds nothing.
	if n, _ := x.d.Drain(context.Background()); n != 0 {
		t.Fatalf("a second pass sent %d deliveries", n)
	}
}

// A receiver that rejects the signature (it has the wrong secret) makes the sender retry; once fixed, it delivers.
func TestARejectedSignatureIsRetriedUntilTheReceiverIsFixed(t *testing.T) {
	logs := captureLogs(t)
	x := startE2E(t, "/hook", false)
	x.teach(t, mustSecret(t)) // a secret that is not the endpoint's
	f := x.f
	create(t, f, f.a.Key, f.customer)

	if _, err := x.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status int
	var lastErr string
	var attempts int
	err := f.env.Pool.QueryRow(context.Background(), `SELECT last_response_status, last_error, attempt_count FROM webhook_deliveries`).Scan(&status, &lastErr, &attempts)
	if err != nil || status != 401 || lastErr != "http 401" || attempts != 1 {
		t.Fatalf("status=%d err=%q attempts=%d (%v)", status, lastErr, attempts, err)
	}
	if !strings.Contains(logs.String(), "signature NOT verified") {
		t.Fatalf("the receiver must say it could not verify: %s", logs.String())
	}

	x.teach(t, x.secret) // fix the receiver
	if _, err := f.env.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := x.statusCounts(t); c["delivered"] != 1 {
		t.Fatalf("after the fix: %v", c)
	}
	if !strings.Contains(logs.String(), `"redelivery":true`) {
		t.Fatal("the receiver should recognise the retry as a redelivery of the same event")
	}
}

func mustSecret(t *testing.T) string {
	t.Helper()
	s, err := webhooks.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// An endpoint that keeps failing is retried on the schedule and then kept as failed; nothing else is affected.
func TestAnEndpointThatKeepsFailingEndsAsFailedWithoutAffectingTheBusiness(t *testing.T) {
	x := startE2E(t, "/hook/status/503", true)
	f := x.f
	inv := openInvoice(t, f)
	if r := pay(t, f, inv.ID, "tok_success"); r.Code != 200 {
		t.Fatalf("a dead webhook endpoint must not affect payments: %d %s", r.Code, r.Body)
	}
	for i := 0; i < 6; i++ {
		if _, err := f.env.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 second' WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		if _, err := x.d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c := x.statusCounts(t); c["failed"] != 2 || len(c) != 1 { // invoice.created and invoice.paid
		t.Fatalf("delivery states: %v", c)
	}
	var maxAttempts int
	if err := f.env.Pool.QueryRow(context.Background(), `SELECT max(attempt_count) FROM webhook_deliveries`).Scan(&maxAttempts); err != nil || maxAttempts != 4 {
		t.Fatalf("attempts = %d (%v), want the schedule's 4", maxAttempts, err)
	}
	if got := getInvoice(t, f, inv.ID); got.Status != "paid" {
		t.Fatalf("the invoice is unaffected: %+v", got)
	}
}
