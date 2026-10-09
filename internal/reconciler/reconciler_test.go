package reconciler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/payments"
	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/testdb"
)

// fakeProvider answers queries from a table, and counts them.
type fakeProvider struct {
	mu      sync.Mutex
	answers map[uuid.UUID]psp.Outcome
	asked   map[uuid.UUID]int
	block   chan struct{} // when set, Query waits for it or the context
}

func newProvider() *fakeProvider {
	return &fakeProvider{answers: map[uuid.UUID]psp.Outcome{}, asked: map[uuid.UUID]int{}}
}

func (f *fakeProvider) Query(ctx context.Context, ref uuid.UUID) psp.Outcome {
	f.mu.Lock()
	f.asked[ref]++
	o, ok := f.answers[ref]
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return psp.Outcome{Status: psp.Unknown, Cause: ctx.Err()}
		}
	}
	if !ok {
		return psp.Outcome{Status: psp.NotFound}
	}
	return o
}

func (f *fakeProvider) timesAsked(ref uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[ref]
}

func (f *fakeProvider) totalAsked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.asked {
		n += c
	}
	return n
}

// recorder is a stand-in listener that records what it was told.
type recorder struct {
	mu   sync.Mutex
	got  map[uuid.UUID]psp.Outcome
	fail error
}

func newRecorder() *recorder { return &recorder{got: map[uuid.UUID]psp.Outcome{}} }

func (r *recorder) ResolvePayment(_ context.Context, ref uuid.UUID, o psp.Outcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.got[ref] = o
	return nil
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

type env struct {
	rec      *Reconciler
	provider *fakeProvider
	listener *recorder
	svc      *payments.Service
	exec     func(sql string, args ...any)
	pending  func() []uuid.UUID
	newOld   func(age time.Duration) uuid.UUID
}

func setup(t *testing.T, minAge time.Duration) *env {
	t.Helper()
	pool := testdb.New(t)
	e := &env{provider: newProvider(), listener: newRecorder(), svc: payments.NewService()}
	e.rec = New(pool, e.provider, e.listener, minAge)
	ctx := context.Background()
	e.exec = func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A pending payment created `age` ago on the database clock.
	e.newOld = func(age time.Duration) uuid.UUID {
		p, err := e.svc.Create(ctx, pool, 100)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE payments SET created_at = now() - ($2::bigint * interval '1 millisecond') WHERE payment_ref_id = $1`, p.RefID, age.Milliseconds())
		return p.RefID
	}
	e.pending = func() []uuid.UUID {
		rows, err := pool.Query(ctx, `SELECT payment_ref_id FROM payments WHERE status = 'pending'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	return e
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&syncWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type syncWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestRecordsDefinitiveAnswersOnly(t *testing.T) {
	e := setup(t, time.Second)
	ok, declined, processing, unknown, missing := e.newOld(time.Minute), e.newOld(time.Minute), e.newOld(time.Minute), e.newOld(time.Minute), e.newOld(time.Minute)
	e.provider.answers[ok] = psp.Outcome{Status: psp.Succeeded, PSPRef: "psp-1"}
	e.provider.answers[declined] = psp.Outcome{Status: psp.Failed, Code: psp.CodeCardDeclined}
	e.provider.answers[processing] = psp.Outcome{Status: psp.Processing}
	e.provider.answers[unknown] = psp.Outcome{Status: psp.Unknown, Cause: errors.New("503")}
	// `missing` has no answer: the provider has never heard of it.

	res, err := e.rec.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Checked: 5, Resolved: 2, Processing: 1, NotFound: 1, Unknown: 1}) {
		t.Fatalf("result = %+v", res)
	}
	if e.listener.count() != 2 || e.listener.got[ok].Status != psp.Succeeded || e.listener.got[declined].Code != psp.CodeCardDeclined {
		t.Fatalf("the listener must be told exactly the two definitive answers: %+v", e.listener.got)
	}
	for _, ref := range []uuid.UUID{processing, unknown, missing} {
		if _, told := e.listener.got[ref]; told {
			t.Errorf("%s was not definitive and must not be resolved", ref)
		}
	}
}

// The inline pay call owns a payment until its own deadline; younger payments are not asked about.
func TestLeavesYoungPaymentsAlone(t *testing.T) {
	e := setup(t, 5*time.Second)
	young, old := e.newOld(500*time.Millisecond), e.newOld(30*time.Second)
	e.provider.answers[young] = psp.Outcome{Status: psp.Succeeded, PSPRef: "a"}
	e.provider.answers[old] = psp.Outcome{Status: psp.Succeeded, PSPRef: "b"}

	if _, err := e.rec.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.provider.timesAsked(young) != 0 {
		t.Fatal("a payment younger than the minimum age must not be queried")
	}
	if e.provider.timesAsked(old) != 1 || e.listener.count() != 1 {
		t.Fatalf("the old payment should be resolved: asked %d", e.provider.timesAsked(old))
	}
}

func TestNeverAsksAboutResolvedPayments(t *testing.T) {
	e := setup(t, time.Second)
	done := e.newOld(time.Minute)
	if _, err := e.svc.Resolve(context.Background(), e.rec.db, done, psp.Outcome{Status: psp.Succeeded, PSPRef: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rec.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.provider.totalAsked() != 0 {
		t.Fatal("resolved payments must not be queried")
	}
}

// A payment the provider has no record of is never failed by inference: it is asked about on every
// sweep, logged at warn each time, and resolves normally the moment the provider knows it.
func TestNotFoundIsRetriedEverySweepAndLoggedAtWarn(t *testing.T) {
	logs := captureLogs(t)
	e := setup(t, time.Second)
	ref := e.newOld(time.Hour) // pending for an hour, and the provider still has nothing

	for sweep := 1; sweep <= 3; sweep++ {
		res, err := e.rec.RunOnce(context.Background())
		if err != nil || res.NotFound != 1 || res.Resolved != 0 {
			t.Fatalf("sweep %d: %+v %v", sweep, res, err)
		}
	}
	if got := e.provider.timesAsked(ref); got != 3 {
		t.Fatalf("asked %d times, want once per sweep", got)
	}
	if n := bytes.Count(logs.Bytes(), []byte(`"level":"WARN"`)); n != 3 {
		t.Fatalf("%d warn lines, want one per sweep: %s", n, logs.String())
	}
	if e.listener.count() != 0 || len(e.pending()) != 1 {
		t.Fatal("an hour of silence must not turn into a failure")
	}

	e.provider.answers[ref] = psp.Outcome{Status: psp.Succeeded, PSPRef: "late"}
	if res, _ := e.rec.RunOnce(context.Background()); res.Resolved != 1 {
		t.Fatalf("once the provider knows it, it resolves: %+v", res)
	}
}

// Payments that stay pending must not push newer ones out of reach.
func TestStuckPaymentsDoNotStarveNewerOnes(t *testing.T) {
	e := setup(t, time.Second)
	e.rec.pageSize = 2
	for i := 0; i < 5; i++ {
		e.newOld(time.Duration(10+i) * time.Hour) // oldest, and unknown to the provider
	}
	var fresh []uuid.UUID
	for i := 0; i < 5; i++ {
		ref := e.newOld(time.Duration(i+1) * time.Minute)
		e.provider.answers[ref] = psp.Outcome{Status: psp.Succeeded, PSPRef: fmt.Sprintf("p%d", i)}
		fresh = append(fresh, ref)
	}

	res, err := e.rec.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 10 || res.Resolved != 5 || res.NotFound != 5 {
		t.Fatalf("one sweep must cover every pending payment across pages of 2: %+v", res)
	}
	for _, ref := range fresh {
		if e.listener.got[ref].Status != psp.Succeeded {
			t.Errorf("%s was never reached", ref)
		}
	}
}

func TestEveryPendingPaymentIsCheckedExactlyOncePerSweepAcrossPages(t *testing.T) {
	e := setup(t, time.Second)
	e.rec.pageSize = 3
	var refs []uuid.UUID
	for i := 0; i < 11; i++ {
		ref := e.newOld(time.Minute + time.Duration(i)*time.Millisecond)
		refs = append(refs, ref)
	}
	// Two payments created at the same instant must not be skipped or repeated at a page boundary.
	e.exec(`UPDATE payments SET created_at = (SELECT created_at FROM payments ORDER BY created_at LIMIT 1) WHERE payment_ref_id = ANY($1)`, refs[:6])

	res, err := e.rec.RunOnce(context.Background())
	if err != nil || res.Checked != 11 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, ref := range refs {
		if got := e.provider.timesAsked(ref); got != 1 {
			t.Errorf("%s asked %d times in one sweep", ref, got)
		}
	}
}

// When the provider is unreachable the sweep reports it and changes nothing; the same
// payments resolve on the first sweep after it recovers.
func TestAnUnreachableProviderChangesNothingAndRecovers(t *testing.T) {
	e := setup(t, time.Second)
	refs := []uuid.UUID{e.newOld(time.Minute), e.newOld(time.Minute), e.newOld(time.Minute)}

	down := psp.Outcome{Status: psp.Unknown, Cause: errors.New("connection refused")}
	for _, ref := range refs {
		e.provider.answers[ref] = down
	}
	for sweep := 0; sweep < 3; sweep++ {
		res, err := e.rec.RunOnce(context.Background())
		if err != nil || res.Unknown != 3 || res.Resolved != 0 {
			t.Fatalf("sweep %d during the outage: %+v %v", sweep, res, err)
		}
	}
	if e.listener.count() != 0 || len(e.pending()) != 3 {
		t.Fatal("an outage must not resolve or fail anything")
	}

	for _, ref := range refs {
		e.provider.answers[ref] = psp.Outcome{Status: psp.Succeeded, PSPRef: "back"}
	}
	if res, _ := e.rec.RunOnce(context.Background()); res.Resolved != 3 {
		t.Fatalf("after recovery: %+v", res)
	}
}

func TestFailureToRecordAnAnswerIsRetriedNextSweep(t *testing.T) {
	logs := captureLogs(t)
	e := setup(t, time.Second)
	ref := e.newOld(time.Minute)
	e.provider.answers[ref] = psp.Outcome{Status: psp.Succeeded, PSPRef: "p"}
	e.listener.fail = errors.New("database unavailable")

	res, _ := e.rec.RunOnce(context.Background())
	if res.Errors != 1 || res.Resolved != 0 {
		t.Fatalf("%+v", res)
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"level":"ERROR"`)) {
		t.Fatal("an unrecordable answer must be an error log")
	}

	e.listener.fail = nil
	if res, _ := e.rec.RunOnce(context.Background()); res.Resolved != 1 {
		t.Fatalf("the next sweep should record it: %+v", res)
	}
}

func TestSweepsAreConcurrentButBounded(t *testing.T) {
	e := setup(t, time.Second)
	e.rec.workers = 4
	var inFlight, peak atomic.Int32
	e.rec.prober = proberFunc(func(ctx context.Context, ref uuid.UUID) psp.Outcome {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return psp.Outcome{Status: psp.Processing}
	})
	for i := 0; i < 20; i++ {
		e.newOld(time.Minute)
	}
	start := time.Now()
	if _, err := e.rec.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p < 2 || p > 4 {
		t.Fatalf("peak concurrency %d, want between 2 and the limit of 4", p)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("20 payments took %v; they should be checked concurrently", time.Since(start))
	}
}

type proberFunc func(ctx context.Context, ref uuid.UUID) psp.Outcome

func (f proberFunc) Query(ctx context.Context, ref uuid.UUID) psp.Outcome { return f(ctx, ref) }

func TestRunSweepsRepeatedlyAndStopsPromptlyOnCancel(t *testing.T) {
	e := setup(t, time.Second)
	ref := e.newOld(time.Minute)
	e.provider.answers[ref] = psp.Outcome{Status: psp.Processing}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.rec.Run(ctx, 40*time.Millisecond); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for e.provider.timesAsked(ref) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.provider.timesAsked(ref); got < 4 {
		t.Fatalf("only %d sweeps in 3s with a 40ms interval", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}

func TestCancellingMidSweepStopsWithoutResolvingAnything(t *testing.T) {
	e := setup(t, time.Second)
	e.rec.workers = 2
	for i := 0; i < 6; i++ {
		ref := e.newOld(time.Minute)
		e.provider.answers[ref] = psp.Outcome{Status: psp.Succeeded, PSPRef: "x"}
	}
	e.provider.block = make(chan struct{}) // every Query hangs until released or cancelled

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.rec.RunOnce(ctx); done <- err }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep did not stop when cancelled")
	}
	if e.listener.count() != 0 {
		t.Fatal("a cancelled sweep must not record answers it did not finish asking for")
	}
	// Only the two in flight were started; once cancelled, no further payments are dispatched.
	if asked := e.provider.totalAsked(); asked > 2 {
		t.Fatalf("%d payments were asked about; a cancelled sweep must stop dispatching (limit 2 in flight)", asked)
	}
}

func TestNoTransactionIsOpenWhileAskingTheProvider(t *testing.T) {
	e := setup(t, time.Second)
	e.newOld(time.Minute)
	release := make(chan struct{})
	e.provider.block = release
	done := make(chan struct{})
	go func() { _, _ = e.rec.RunOnce(context.Background()); close(done) }()
	time.Sleep(150 * time.Millisecond)

	var open int
	err := e.rec.db.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = current_setting('application_name')
		  AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open)
	if err != nil || open != 0 {
		t.Fatalf("%d transactions open while the provider was being asked (err %v)", open, err)
	}
	close(release)
	<-done
}
