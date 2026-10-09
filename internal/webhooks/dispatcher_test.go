package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- a receiver that records what it was sent ----------------------------------

type hit struct {
	ID, Timestamp, Signature, ContentType, UserAgent string
	Body                                             []byte
	At                                               time.Time
}

type target struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []hit
}

// newTarget serves handler(n, w, r) where n is the 1-based number of this request.
func newTarget(t testing.TB, handler func(n int, w http.ResponseWriter, r *http.Request)) *target {
	t.Helper()
	tg := &target{}
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tg.mu.Lock()
		tg.hits = append(tg.hits, hit{
			ID: r.Header.Get(HeaderID), Timestamp: r.Header.Get(HeaderTimestamp), Signature: r.Header.Get(HeaderSignature),
			ContentType: r.Header.Get("Content-Type"), UserAgent: r.Header.Get("User-Agent"), Body: body, At: time.Now(),
		})
		n := len(tg.hits)
		tg.mu.Unlock()
		if handler != nil {
			handler(n, w, r)
		}
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *target) count() int { tg.mu.Lock(); defer tg.mu.Unlock(); return len(tg.hits) }
func (tg *target) all() []hit {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]hit(nil), tg.hits...)
}

func respond(status int) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }
}

// ---- helpers --------------------------------------------------------------------

type dispEnv struct {
	outboxEnv
}

func setupDisp(t *testing.T) dispEnv { return dispEnv{setupOutbox(t)} }

// endpoint registers an endpoint for business A pointing at url and returns it with its secret.
func (e dispEnv) endpoint(t *testing.T, url string) endpointJSON {
	t.Helper()
	return register(t, e.env, e.a.Key, url)
}

func (e dispEnv) newDispatcher(schedule []time.Duration, timeout time.Duration) *Dispatcher {
	d := NewDispatcher(e.env.Pool, schedule, timeout)
	d.jitter = func() float64 { return 0 }
	return d
}

func (e dispEnv) event(t *testing.T, eventType string) {
	t.Helper()
	if err := e.enqueue(t, e.a.BusinessID, eventType, fixedObject(map[string]any{"id": uuid.NewString(), "status": "open"})); err != nil {
		t.Fatal(err)
	}
}

type rowState struct {
	Status       string
	Attempts     int
	NextAt       time.Time
	LastAt       *time.Time
	LastStatus   *int
	LastError    *string
	DeliveredAt  *time.Time
	PayloadText  string
	EventID, EID string
}

func (e dispEnv) rows(t *testing.T) []rowState {
	t.Helper()
	q, err := e.env.Pool.Query(context.Background(),
		`SELECT status, attempt_count, next_attempt_at, last_attempt_at, last_response_status, last_error, delivered_at, payload::text, event_id, endpoint_id::text
		 FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var out []rowState
	for q.Next() {
		var r rowState
		if err := q.Scan(&r.Status, &r.Attempts, &r.NextAt, &r.LastAt, &r.LastStatus, &r.LastError, &r.DeliveredAt, &r.PayloadText, &r.EventID, &r.EID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e dispEnv) one(t *testing.T) rowState {
	t.Helper()
	r := e.rows(t)
	if len(r) != 1 {
		t.Fatalf("got %d rows, want 1", len(r))
	}
	return r[0]
}

func (e dispEnv) setDue(t *testing.T, delta string) {
	t.Helper()
	if _, err := e.env.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at = now() + $1::interval`, delta); err != nil {
		t.Fatal(err)
	}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func schedule(delays ...time.Duration) []time.Duration { return delays }

// referenceSignature is a second, independent implementation of the scheme used to check Sign.
func referenceSignature(t *testing.T, secret, id string, ts string, body []byte) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id + "." + ts + "."))
	h.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ---- delivery and signing ---------------------------------------------------------

func TestDeliversEachEventSignedWithItsEndpointsOwnSecret(t *testing.T) {
	e := setupDisp(t)
	t1, t2 := newTarget(t, respond(200)), newTarget(t, respond(200))
	ep1, ep2 := e.endpoint(t, t1.srv.URL+"/a"), e.endpoint(t, t2.srv.URL+"/b")
	e.event(t, EventInvoicePaid)

	if n, err := e.newDispatcher(schedule(0, ms(50)), 2*time.Second).Drain(context.Background()); err != nil || n != 2 {
		t.Fatalf("attempted %d (err %v), want 2", n, err)
	}

	stored := e.rows(t)
	for _, tc := range []struct {
		tg, other *target
		ep, oep   endpointJSON
	}{{t1, t2, ep1, ep2}, {t2, t1, ep2, ep1}} {
		hits := tc.tg.all()
		if len(hits) != 1 {
			t.Fatalf("endpoint got %d requests, want 1", len(hits))
		}
		h := hits[0]
		if h.ContentType != "application/json" || h.UserAgent == "" {
			t.Errorf("headers: content-type %q, user-agent %q", h.ContentType, h.UserAgent)
		}
		if h.ID != stored[0].EventID || !strings.HasPrefix(h.ID, "evt_") {
			t.Errorf("webhook-id %q should be the event id %q", h.ID, stored[0].EventID)
		}
		ts, err := strconv.ParseInt(h.Timestamp, 10, 64)
		if err != nil || time.Since(time.Unix(ts, 0)) > 10*time.Second || time.Until(time.Unix(ts, 0)) > 10*time.Second {
			t.Errorf("webhook-timestamp %q should be the current Unix time", h.Timestamp)
		}
		// The signature verifies with this endpoint's own secret, by our verifier and by an independent one...
		if err := Verify([]string{tc.ep.Secret}, h.ID, h.Timestamp, h.Signature, h.Body, time.Now()); err != nil {
			t.Errorf("signature does not verify with the endpoint's secret: %v", err)
		}
		if want := referenceSignature(t, tc.ep.Secret, h.ID, h.Timestamp, h.Body); h.Signature != want {
			t.Errorf("signature %q != independently computed %q", h.Signature, want)
		}
		// ...and not with the other endpoint's.
		if err := Verify([]string{tc.oep.Secret}, h.ID, h.Timestamp, h.Signature, h.Body, time.Now()); err == nil {
			t.Error("a signature must not verify with another endpoint's secret")
		}
	}

	// The bytes sent are exactly the stored event body.
	if string(t1.all()[0].Body) != stored[0].PayloadText {
		t.Fatalf("body sent differs from the stored payload:\n sent: %s\n kept: %s", t1.all()[0].Body, stored[0].PayloadText)
	}
	for _, r := range stored {
		if r.Status != "delivered" || r.Attempts != 1 || r.DeliveredAt == nil || r.LastStatus == nil || *r.LastStatus != 200 || r.LastError != nil {
			t.Fatalf("row after delivery: %+v", r)
		}
	}
}

func TestOnlyA2xxAnswerIsADelivery(t *testing.T) {
	cases := []struct {
		status    int
		delivered bool
	}{
		{200, true}, {201, true}, {202, true}, {204, true}, {299, true},
		{199 + 1, true},
		{301, false}, {302, false}, {307, false},
		{400, false}, {401, false}, {404, false}, {410, false}, {429, false},
		{500, false}, {502, false}, {503, false}, {504, false},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			e := setupDisp(t)
			tg := newTarget(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				if c.status/100 == 3 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(c.status)
			})
			e.endpoint(t, tg.srv.URL)
			e.event(t, EventInvoicePaid)
			if _, err := e.newDispatcher(schedule(0, time.Hour), 2*time.Second).Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			r := e.one(t)
			if c.delivered {
				if r.Status != "delivered" || r.DeliveredAt == nil {
					t.Fatalf("%+v", r)
				}
				return
			}
			if r.Status != "pending" || r.Attempts != 1 || r.LastStatus == nil || *r.LastStatus != c.status ||
				r.LastError == nil || !strings.HasPrefix(*r.LastError, fmt.Sprintf("http %d", c.status)) {
				t.Fatalf("a %d is not a delivery: %+v", c.status, r)
			}
			if time.Until(r.NextAt) < 50*time.Minute {
				t.Fatalf("the retry should be scheduled an hour out, next attempt in %v", time.Until(r.NextAt))
			}
		})
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	e := setupDisp(t)
	elsewhere := newTarget(t, respond(200))
	redirector := newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/stolen", http.StatusTemporaryRedirect)
	})
	e.endpoint(t, redirector.srv.URL)
	e.event(t, EventInvoicePaid)
	if _, err := e.newDispatcher(schedule(0, time.Hour), 2*time.Second).Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elsewhere.count() != 0 {
		t.Fatal("a signed body was sent to a redirect target the business never registered")
	}
	if r := e.one(t); r.Status != "pending" || r.LastError == nil || !strings.Contains(*r.LastError, "redirects are not followed") {
		t.Fatalf("%+v", r)
	}
}

func TestTransportFailuresAreRetriedAndClassified(t *testing.T) {
	t.Run("timeout is bounded", func(t *testing.T) {
		e := setupDisp(t)
		tg := newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		e.endpoint(t, tg.srv.URL)
		e.event(t, EventInvoicePaid)
		start := time.Now()
		if _, err := e.newDispatcher(schedule(0, time.Hour), 200*time.Millisecond).Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if el := time.Since(start); el < 150*time.Millisecond || el > 2*time.Second {
			t.Fatalf("attempt took %v, want about the 200ms timeout", el)
		}
		if r := e.one(t); r.Status != "pending" || r.LastError == nil || *r.LastError != "timeout" || r.LastStatus != nil {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		e := setupDisp(t)
		tg := newTarget(t, nil)
		url := tg.srv.URL
		tg.srv.Close()
		e.endpoint(t, url)
		e.event(t, EventInvoicePaid)
		if _, err := e.newDispatcher(schedule(0, time.Hour), time.Second).Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := e.one(t); r.Status != "pending" || r.LastError == nil || *r.LastError != "connection error" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("connection dropped without an answer", func(t *testing.T) {
		e := setupDisp(t)
		tg := newTarget(t, func(int, http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
		e.endpoint(t, tg.srv.URL)
		e.event(t, EventInvoicePaid)
		if _, err := e.newDispatcher(schedule(0, time.Hour), time.Second).Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := e.one(t); r.Status != "pending" || r.LastError == nil || *r.LastError != "connection error" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("an endless response body does not hang the worker", func(t *testing.T) {
		e := setupDisp(t)
		tg := newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			chunk := make([]byte, 64<<10)
			for r.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		})
		e.endpoint(t, tg.srv.URL)
		e.event(t, EventInvoicePaid)
		start := time.Now()
		if _, err := e.newDispatcher(schedule(0, time.Hour), 2*time.Second).Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("took %v: the response body must be read only up to a limit", time.Since(start))
		}
		if r := e.one(t); r.Status != "delivered" {
			t.Fatalf("a 2xx is a delivery whatever the body: %+v", r)
		}
	})
}

// ---- retries and backoff ----------------------------------------------------------

func TestBackoffFollowsTheScheduleWithJitterBounds(t *testing.T) {
	d := NewDispatcher(nil, schedule(0, 30*time.Second, 2*time.Minute, 10*time.Minute, time.Hour, 6*time.Hour, 12*time.Hour), time.Second)
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 12 * time.Hour}
	distinct := map[time.Duration]bool{}
	for n := 1; n <= 6; n++ {
		base := want[n-1]
		for i := 0; i < 2000; i++ {
			got := d.backoff(n)
			if got < time.Duration(float64(base)*0.8)-1 || got > time.Duration(float64(base)*1.2)+1 {
				t.Fatalf("after attempt %d: %v is outside ±20%% of %v", n, got, base)
			}
			distinct[got] = true
		}
	}
	if len(distinct) < 1000 {
		t.Fatalf("only %d distinct delays: the jitter does not spread retries", len(distinct))
	}
	// 7 attempts over 19h12m30s before jitter (DESIGN.md section 4 says "about 19.7 hours",
	// which does not match the schedule it lists; the schedule is what is implemented).
	var span time.Duration
	for _, w := range want {
		span += w
	}
	if span != 19*time.Hour+12*time.Minute+30*time.Second {
		t.Fatalf("the default schedule spans %v, want 19h12m30s", span)
	}
}

func TestRetryDelayIsAppliedAfterEachFailure(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(500))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, 400*time.Millisecond, 800*time.Millisecond, 1600*time.Millisecond), time.Second)

	for attempt, wantDelay := range []time.Duration{400 * time.Millisecond, 800 * time.Millisecond} {
		e.setDue(t, "-1 second")
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		r := e.one(t)
		if r.Status != "pending" || r.Attempts != attempt+1 || r.LastAt == nil {
			t.Fatalf("after attempt %d: %+v", attempt+1, r)
		}
		gap := r.NextAt.Sub(*r.LastAt)
		if gap < wantDelay-80*time.Millisecond || gap > wantDelay+80*time.Millisecond {
			t.Fatalf("after attempt %d the retry is %v away, want about %v", attempt+1, gap, wantDelay)
		}
	}
}

func TestJitterIsAppliedToTheRecordedDelay(t *testing.T) {
	for _, j := range []float64{-0.2, 0, 0.2} {
		e := setupDisp(t)
		tg := newTarget(t, respond(500))
		e.endpoint(t, tg.srv.URL)
		e.event(t, EventInvoicePaid)
		d := e.newDispatcher(schedule(0, time.Second), time.Second)
		d.schedule = schedule(0, 10*time.Second, 20*time.Second)
		d.jitter = func() float64 { return j }
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		r := e.one(t)
		want := time.Duration(float64(10*time.Second) * (1 + j))
		if gap := r.NextAt.Sub(*r.LastAt); gap < want-100*time.Millisecond || gap > want+100*time.Millisecond {
			t.Fatalf("jitter %+.1f: retry in %v, want about %v", j, gap, want)
		}
	}
}

func TestDeliverySucceedsOnALaterAttemptWithAFreshSignatureEachTime(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	ep := e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(20), ms(20), ms(20), ms(20)), time.Second)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { clock = clock.Add(10 * time.Second); return clock }

	for i := 0; i < 3; i++ {
		e.setDue(t, "-1 second")
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r := e.one(t)
	if r.Status != "delivered" || r.Attempts != 3 || r.LastError != nil {
		t.Fatalf("%+v", r)
	}
	hits := tg.all()
	if len(hits) != 3 {
		t.Fatalf("%d requests, want 3", len(hits))
	}
	seenTS := map[string]bool{}
	for i, h := range hits {
		if h.ID != r.EventID {
			t.Errorf("attempt %d: webhook-id %q changed; it must be stable across retries so receivers can deduplicate", i+1, h.ID)
		}
		if seenTS[h.Timestamp] {
			t.Errorf("attempt %d reused timestamp %s", i+1, h.Timestamp)
		}
		seenTS[h.Timestamp] = true
		if err := Verify([]string{ep.Secret}, h.ID, h.Timestamp, h.Signature, h.Body, time.Unix(mustInt(t, h.Timestamp), 0)); err != nil {
			t.Errorf("attempt %d does not verify: %v", i+1, err)
		}
		if i > 0 && h.Signature == hits[i-1].Signature {
			t.Errorf("attempt %d has the same signature as the previous one", i+1)
		}
		// A signature is bound to its own timestamp: replaying it with another one fails.
		if err := Verify([]string{ep.Secret}, h.ID, hits[(i+1)%3].Timestamp, h.Signature, h.Body, time.Unix(mustInt(t, hits[(i+1)%3].Timestamp), 0)); err == nil {
			t.Errorf("attempt %d: signature verified under a different timestamp", i+1)
		}
		if string(h.Body) != string(hits[0].Body) {
			t.Errorf("attempt %d sent a different body", i+1)
		}
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAttemptBudgetIsTheScheduleLengthThenTheRowIsKeptAsFailed(t *testing.T) {
	for _, attempts := range []int{1, 3, 7} {
		t.Run(fmt.Sprintf("%d attempts", attempts), func(t *testing.T) {
			e := setupDisp(t)
			tg := newTarget(t, respond(500))
			e.endpoint(t, tg.srv.URL)
			e.event(t, EventInvoicePaid)
			sched := make([]time.Duration, attempts)
			for i := 1; i < attempts; i++ {
				sched[i] = ms(10)
			}
			d := e.newDispatcher(sched, time.Second)
			logs := captureLogs(t)

			for i := 0; i < attempts+4; i++ { // more passes than attempts: the extras must find nothing to do
				e.setDue(t, "-1 second")
				if _, err := d.RunOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			r := e.one(t)
			if r.Status != "failed" || r.Attempts != attempts || r.LastError == nil || *r.LastError != "http 500" {
				t.Fatalf("%+v", r)
			}
			if tg.count() != attempts {
				t.Fatalf("%d requests, want exactly %d", tg.count(), attempts)
			}
			if !strings.Contains(logs.String(), "failed for good") {
				t.Fatal("giving up must be an error log")
			}
		})
	}
}

// ---- leases and crashes -----------------------------------------------------------

func TestAClaimedDeliveryIsHiddenUntilItsLeaseExpires(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10), ms(10), ms(10)), time.Second)

	// A worker claims the row and dies before sending anything.
	got, err := d.claim(context.Background(), 10, nil)
	if err != nil || len(got) != 1 || got[0].attempt != 1 {
		t.Fatalf("claim: %v %v", got, err)
	}
	r := e.one(t)
	if r.Attempts != 1 || time.Until(r.NextAt) < 25*time.Second || time.Until(r.NextAt) > 31*time.Second {
		t.Fatalf("a claim must count the attempt and lease the row for about 30s: attempts=%d, next in %v", r.Attempts, time.Until(r.NextAt))
	}
	if n, _ := d.RunOnce(context.Background()); n != 0 || tg.count() != 0 {
		t.Fatal("another worker must not take a leased delivery")
	}

	// Once the lease has run out the delivery is retried, and the lost attempt stays used up.
	e.setDue(t, "-1 second")
	if n, _ := d.RunOnce(context.Background()); n != 1 {
		t.Fatalf("attempted %d, want 1", n)
	}
	if r := e.one(t); r.Status != "delivered" || r.Attempts != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestACrashOnTheLastAttemptEndsAsFailedWithoutAnotherSend(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10), ms(10)), time.Second) // 3 attempts

	if _, err := e.env.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET attempt_count = 3, next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := d.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("attempted %d (err %v): the budget is spent", n, err)
	}
	r := e.one(t)
	if r.Status != "failed" || tg.count() != 0 || r.LastError == nil || !strings.Contains(*r.LastError, "worker stopped") {
		t.Fatalf("%+v hits=%d", r, tg.count())
	}
}

func TestAStaleResultCannotOverwriteANewerAttempt(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10), ms(10), ms(10)), time.Second)

	first, _ := d.claim(context.Background(), 1, nil) // attempt 1, then the worker stalls...
	e.setDue(t, "-1 second")
	second, _ := d.claim(context.Background(), 1, nil) // ...the lease lapses and attempt 2 starts elsewhere
	if len(first) != 1 || len(second) != 1 || second[0].attempt != 2 {
		t.Fatalf("claims: %v %v", first, second)
	}
	// The stalled worker finally reports success for attempt 1.
	if err := d.record(context.Background(), first[0], outcome{ok: true, status: 200}); err != nil {
		t.Fatal(err)
	}
	if r := e.one(t); r.Status != "pending" || r.Attempts != 2 {
		t.Fatalf("attempt 1's late result must not touch the row now owned by attempt 2: %+v", r)
	}
	if err := d.record(context.Background(), second[0], outcome{ok: true, status: 200}); err != nil {
		t.Fatal(err)
	}
	if r := e.one(t); r.Status != "delivered" {
		t.Fatalf("%+v", r)
	}
}

func TestShuttingDownMidAttemptRecordsNothingAndTheDeliveryIsRetriedLater(t *testing.T) {
	e := setupDisp(t)
	started := make(chan struct{}, 1)
	tg := newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10), ms(10)), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, 20*time.Millisecond); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the dispatcher did not stop on shutdown")
	}
	r := e.one(t)
	if r.Status != "pending" || r.Attempts != 1 || r.LastError != nil || r.LastStatus != nil {
		t.Fatalf("an interrupted attempt must leave no result, only the used-up attempt and the lease: %+v", r)
	}
	if time.Until(r.NextAt) < 25*time.Second {
		t.Fatalf("the lease must still be in place (next in %v)", time.Until(r.NextAt))
	}
}

// ---- disabled endpoints, concurrency, fairness -----------------------------------------

func TestDisabledEndpointsAreNotAttemptedAndResumeWhenEnabled(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	ep := e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10)), time.Second)

	if _, err := e.env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Drain(context.Background()); n != 0 || tg.count() != 0 {
		t.Fatalf("a disabled endpoint was attempted (%d, %d requests)", n, tg.count())
	}
	if r := e.one(t); r.Status != "pending" || r.Attempts != 0 {
		t.Fatalf("the row must stay pending and untouched: %+v", r)
	}

	if _, err := e.env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET disabled_at = NULL WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Drain(context.Background()); n != 1 || e.one(t).Status != "delivered" {
		t.Fatalf("after re-enabling it must be delivered")
	}
}

// Several workers (several processes, in production) share the outbox; no delivery may be sent twice.
func TestConcurrentDispatchersAttemptEachDeliveryExactlyOnce(t *testing.T) {
	e := setupDisp(t)
	const events = 60
	var mu sync.Mutex
	seen := map[string]int{} // endpoint+event -> requests
	mk := func(name string) *target {
		return newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name+"/"+r.Header.Get(HeaderID)]++
			mu.Unlock()
			w.WriteHeader(200)
		})
	}
	a, b := mk("a"), mk("b")
	e.endpoint(t, a.srv.URL)
	e.endpoint(t, b.srv.URL)
	for i := 0; i < events; i++ {
		e.event(t, EventInvoicePaid)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		d := e.newDispatcher(schedule(0, ms(10)), 2*time.Second)
		go func() {
			defer wg.Done()
			<-start
			if _, err := d.Drain(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(seen) != events*2 {
		t.Fatalf("%d distinct (endpoint, event) pairs were delivered, want %d", len(seen), events*2)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s was sent %d times by 4 competing workers", k, n)
		}
	}
	for _, r := range e.rows(t) {
		if r.Status != "delivered" || r.Attempts != 1 {
			t.Fatalf("%+v", r)
		}
	}
}

// One dead endpoint with a long backlog must not delay a healthy one.
func TestASlowEndpointDoesNotStallAHealthyOne(t *testing.T) {
	e := setupDisp(t)
	slow := newTarget(t, func(_ int, w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	fast := newTarget(t, respond(200))
	e.endpoint(t, slow.srv.URL)
	e.endpoint(t, fast.srv.URL)
	const n = 30
	for i := 0; i < n; i++ {
		e.event(t, EventInvoicePaid)
	}
	d := e.newDispatcher(schedule(0, time.Hour), 300*time.Millisecond) // the dead endpoint costs 300ms per attempt

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx, 20*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	start := time.Now()
	for fast.count() < n {
		if time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("only %d of %d healthy deliveries after %v while the other endpoint hung: head-of-line blocking", fast.count(), n, time.Since(start))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if slow.count() >= n {
		t.Fatal("the slow endpoint should still be working through its backlog, not finished")
	}
}

func TestAnEndpointNeverHasMoreThanAFewDeliveriesInFlight(t *testing.T) {
	e := setupDisp(t)
	var cur, peak atomic.Int32
	tg := newTarget(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		cur.Add(-1)
		w.WriteHeader(200)
	})
	e.endpoint(t, tg.srv.URL)
	for i := 0; i < 40; i++ {
		e.event(t, EventInvoicePaid)
	}
	if _, err := e.newDispatcher(schedule(0, ms(10)), 2*time.Second).Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p < 2 || p > 3 {
		t.Fatalf("peak concurrency to one endpoint was %d, want 2 or 3 (limit %d)", p, perEndpoint)
	}
	if tg.count() != 40 {
		t.Fatalf("%d delivered", tg.count())
	}
}

// ---- hygiene ------------------------------------------------------------------------------

func TestSecretsAndEndpointURLsNeverReachLogsOrResults(t *testing.T) {
	logs := captureLogs(t)
	e := setupDisp(t)
	tg := newTarget(t, respond(500))
	ep := e.endpoint(t, tg.srv.URL+"/hook?token=SUPERSECRETTOKEN")
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10)), time.Second)
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.setDue(t, "-1 second")
	_, _ = d.RunOnce(context.Background())

	out := logs.String()
	for _, banned := range []string{ep.Secret, "SUPERSECRETTOKEN", strings.TrimPrefix(ep.Secret, "whsec_")} {
		if strings.Contains(out, banned) {
			t.Errorf("log output contains %q", banned)
		}
	}
	if !strings.Contains(out, "will retry") {
		t.Fatalf("expected the failed attempt to be logged: %s", out)
	}
	r := e.one(t)
	if r.LastError != nil && strings.Contains(*r.LastError, "SUPERSECRETTOKEN") {
		t.Fatal("the stored error contains the endpoint URL")
	}
}

func TestMalformedStoredSecretFailsTheAttemptWithoutSending(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	ep := e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	if _, err := e.env.Pool.Exec(context.Background(), `UPDATE webhook_endpoints SET secret = 'garbage' WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.newDispatcher(schedule(0, time.Hour), time.Second).Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := e.one(t); tg.count() != 0 || r.Status != "pending" || r.LastError == nil || *r.LastError != "invalid signing secret" {
		t.Fatalf("an unsignable delivery must not be sent unsigned: %+v hits=%d", r, tg.count())
	}
}

func TestDeliveredRowsAreNeverAttemptedAgain(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	d := e.newDispatcher(schedule(0, ms(10)), time.Second)
	d.Drain(context.Background()) //nolint:errcheck
	e.setDue(t, "-1 hour")
	if n, _ := d.Drain(context.Background()); n != 0 || tg.count() != 1 {
		t.Fatalf("a delivered row was attempted again (%d, %d requests)", n, tg.count())
	}
}

// A worker must step around deliveries another worker is holding instead of waiting for them.
func TestClaimingSkipsRowsLockedByAnotherWorker(t *testing.T) {
	e := setupDisp(t)
	tg := newTarget(t, respond(200))
	e.endpoint(t, tg.srv.URL)
	e.event(t, EventInvoicePaid)
	e.event(t, EventInvoicePaid)
	ctx := context.Background()

	holder, err := e.env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var locked uuid.UUID
	if err := holder.QueryRow(ctx, `SELECT id FROM webhook_deliveries ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	d := e.newDispatcher(schedule(0, ms(10)), time.Second)
	type result struct {
		rows []claimed
		err  error
	}
	done := make(chan result, 1)
	go func() { rows, err := d.claim(ctx, 10, nil); done <- result{rows, err} }()
	select {
	case r := <-done:
		if r.err != nil || len(r.rows) != 1 || r.rows[0].id == locked {
			t.Fatalf("claimed %v (err %v): want only the unlocked row", r.rows, r.err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("claim blocked on a row locked by another worker; it must skip it")
	}
}
