package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/db"
)

const (
	defaultWorkers = 10
	// perEndpoint limits how many deliveries to one endpoint run at once (a claim
	// takes at most this many, and an endpoint at the limit is skipped), so a slow
	// or dead endpoint cannot hold every worker and stall the healthy ones.
	perEndpoint = 2
	// lease is how long a claimed delivery is hidden from other workers. It is far
	// longer than a request can take, so a delivery is only retried early if the
	// worker died mid-attempt.
	lease          = 30 * time.Second
	jitterFraction = 0.2
	maxDrainBytes  = 1 << 20
	connectTimeout = 2 * time.Second
	userAgent      = "invoice-service-webhooks/1"
)

// Dispatcher delivers queued webhook events: it claims due rows, signs and POSTs
// each one, and records the outcome. Delivery is at-least-once and unordered.
type Dispatcher struct {
	db       db.Querier
	client   *http.Client
	schedule []time.Duration // delay before each attempt; its length is the attempt budget

	workers int

	mu       sync.Mutex
	inflight map[uuid.UUID]int // deliveries running per endpoint, in this process
	total    int
	wg       sync.WaitGroup
	wake     chan struct{} // signalled when a delivery finishes and frees a slot

	// replaceable in tests
	now    func() time.Time
	jitter func() float64 // a value in [-jitterFraction, +jitterFraction]
}

// NewDispatcher builds a worker that makes at most len(schedule) attempts per
// delivery, each bounded by httpTimeout.
func NewDispatcher(q db.Querier, schedule []time.Duration, httpTimeout time.Duration) *Dispatcher {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: min(connectTimeout, httpTimeout)}).DialContext,
		TLSHandshakeTimeout: min(connectTimeout, httpTimeout),
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Dispatcher{
		db: q,
		client: &http.Client{
			Transport: transport,
			Timeout:   httpTimeout,
			// A redirect is not a delivery: following it could send a signed body somewhere
			// the business never registered. The 3xx is reported as a failed attempt.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		schedule: schedule,
		workers:  defaultWorkers,
		inflight: map[uuid.UUID]int{},
		wake:     make(chan struct{}, 1),
		now:      time.Now,
		jitter:   func() float64 { return (rand.Float64()*2 - 1) * jitterFraction },
	}
}

func (d *Dispatcher) maxAttempts() int { return len(d.schedule) }

// Run delivers until ctx is cancelled. Deliveries run concurrently, a few per
// endpoint at most; whenever one finishes, more are claimed at once, and when
// nothing is due it polls every interval. On shutdown it waits for the attempts
// already in flight, which are abandoned (not recorded) so they retry later.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	slog.InfoContext(ctx, "webhook dispatcher started", "poll_interval", interval.String(), "max_attempts", d.maxAttempts())
	defer slog.InfoContext(context.WithoutCancel(ctx), "webhook dispatcher stopped")

	for {
		if err := d.abandonExhausted(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "webhook dispatch failed", "error", err)
		}
		for ctx.Err() == nil {
			n, err := d.fill(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "webhook dispatch failed", "error", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-d.wake:
		case <-time.After(interval):
		}
	}
}

// RunOnce makes one pass: it abandons deliveries that ran out of attempts, claims
// what is due (within the concurrency limits) and waits for those attempts to
// finish. It returns how many it attempted; Drain repeats until nothing is due.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	if err := d.abandonExhausted(ctx); err != nil {
		return 0, err
	}
	total := 0
	for ctx.Err() == nil {
		n, err := d.fill(ctx)
		if err != nil {
			d.wg.Wait()
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	d.wg.Wait()
	return total, ctx.Err()
}

// Drain runs passes until no delivery is due.
func (d *Dispatcher) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := d.RunOnce(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// fill claims as many due deliveries as there are free slots and starts them.
func (d *Dispatcher) fill(ctx context.Context) (int, error) {
	d.mu.Lock()
	free := d.workers - d.total
	busy := []uuid.UUID{} // never nil: a nil slice is sent as NULL, and "<> ALL(NULL)" matches nothing
	for id, n := range d.inflight {
		if n >= perEndpoint {
			busy = append(busy, id)
		}
	}
	d.mu.Unlock()
	if free <= 0 {
		return 0, nil
	}

	batch, err := d.claim(ctx, free, busy)
	if err != nil {
		return 0, err
	}
	for _, c := range batch {
		d.mu.Lock()
		d.inflight[c.endpointID]++
		d.total++
		d.mu.Unlock()
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.deliver(ctx, c)
			d.mu.Lock()
			if d.inflight[c.endpointID]--; d.inflight[c.endpointID] <= 0 {
				delete(d.inflight, c.endpointID)
			}
			d.total--
			d.mu.Unlock()
			select {
			case d.wake <- struct{}{}:
			default:
			}
		}()
	}
	return len(batch), nil
}

type claimed struct {
	id         uuid.UUID
	eventID    string
	endpointID uuid.UUID
	payload    []byte
	attempt    int // 1-based number of this attempt
	url        string
	secret     string
}

// abandonExhausted marks as failed any delivery whose last attempt was claimed but
// never recorded (the worker died mid-attempt), once its lease has run out.
func (d *Dispatcher) abandonExhausted(ctx context.Context) error {
	_, err := d.db.Exec(ctx,
		`UPDATE webhook_deliveries
		 SET status = 'failed', last_error = COALESCE(last_error, 'worker stopped during the final attempt')
		 WHERE status = 'pending' AND attempt_count >= $1 AND next_attempt_at <= now()`,
		d.maxAttempts())
	return err
}

// claim takes up to limit due deliveries in one statement: it picks candidates (at
// most perEndpoint per endpoint, oldest first, skipping the endpoints in busy),
// locks them with SKIP LOCKED so
// concurrent workers never take the same row, and bumps attempt_count and the
// lease right away. Counting the attempt at claim time means a crash mid-send
// still uses up retry budget instead of looping forever. Deliveries of disabled
// endpoints are not claimed.
func (d *Dispatcher) claim(ctx context.Context, limit int, busy []uuid.UUID) ([]claimed, error) {
	if busy == nil {
		busy = []uuid.UUID{}
	}
	rows, err := d.db.Query(ctx, `
		WITH cand AS (
			SELECT id FROM (
				SELECT w.id, w.next_attempt_at,
				       row_number() OVER (PARTITION BY w.endpoint_id ORDER BY w.next_attempt_at, w.id) AS rn
				FROM webhook_deliveries w
				JOIN webhook_endpoints e ON e.id = w.endpoint_id
				WHERE w.status = 'pending' AND w.next_attempt_at <= now()
				  AND w.attempt_count < $1 AND e.disabled_at IS NULL AND e.id <> ALL($5::uuid[])
			) ranked
			WHERE rn <= $2
			ORDER BY next_attempt_at, id
			LIMIT $3
		), locked AS (
			-- FOR UPDATE cannot be combined with the window function above, so lock afterwards,
			-- re-checking that the row is still due.
			SELECT w.id FROM webhook_deliveries w
			WHERE w.id IN (SELECT id FROM cand) AND w.status = 'pending' AND w.next_attempt_at <= now()
			FOR UPDATE OF w SKIP LOCKED
		)
		UPDATE webhook_deliveries w
		SET attempt_count = w.attempt_count + 1,
		    next_attempt_at = now() + ($4::bigint * interval '1 millisecond'),
		    last_attempt_at = now()
		FROM locked, webhook_endpoints e
		WHERE w.id = locked.id AND e.id = w.endpoint_id
		RETURNING w.id, w.event_id, w.endpoint_id, w.payload::text, w.attempt_count, e.url, e.secret`,
		d.maxAttempts(), perEndpoint, limit, lease.Milliseconds(), busy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		var c claimed
		var payload string
		if err := rows.Scan(&c.id, &c.eventID, &c.endpointID, &payload, &c.attempt, &c.url, &c.secret); err != nil {
			return nil, err
		}
		c.payload = []byte(payload)
		out = append(out, c)
	}
	return out, rows.Err()
}

type outcome struct {
	ok     bool
	status int    // HTTP status, 0 if none was received
	reason string // short classification; never a URL or a response body
}

func (d *Dispatcher) deliver(ctx context.Context, c claimed) {
	res := d.send(ctx, c)
	if ctx.Err() != nil {
		// Shutting down mid-attempt: record nothing. The attempt was already counted and
		// the lease will expire, so the delivery is retried rather than failed.
		return
	}
	if err := d.record(ctx, c, res); err != nil {
		slog.ErrorContext(ctx, "could not record a webhook delivery result; it will be retried after the lease",
			"delivery_id", c.id, "event_id", c.eventID, "error", err)
	}
}

// send signs and POSTs one attempt. Only a 2xx response is a success.
func (d *Dispatcher) send(ctx context.Context, c claimed) outcome {
	timestamp := d.now().Unix()
	signature, err := Sign(c.secret, c.eventID, timestamp, c.payload)
	if err != nil {
		return outcome{reason: "invalid signing secret"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(c.payload))
	if err != nil {
		return outcome{reason: "invalid endpoint url"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderID, c.eventID)
	req.Header.Set(HeaderTimestamp, fmt.Sprint(timestamp))
	req.Header.Set(HeaderSignature, signature)

	resp, err := d.client.Do(req)
	if err != nil {
		if isTimeout(err) {
			return outcome{reason: "timeout"}
		}
		return outcome{reason: "connection error"}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) // lets the connection be reused

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return outcome{ok: true, status: resp.StatusCode}
	}
	reason := fmt.Sprintf("http %d", resp.StatusCode)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		reason += " (redirects are not followed)"
	}
	return outcome{status: resp.StatusCode, reason: reason}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// record stores the result of attempt c.attempt. Every update is conditioned on the
// row still being pending at that attempt number, so a result that arrives after the
// lease expired and another worker took over cannot overwrite the newer attempt.
func (d *Dispatcher) record(ctx context.Context, c claimed, res outcome) error {
	if res.ok {
		_, err := d.db.Exec(ctx,
			`UPDATE webhook_deliveries
			 SET status = 'delivered', delivered_at = now(), last_response_status = $3, last_error = NULL
			 WHERE id = $1 AND status = 'pending' AND attempt_count = $2`, c.id, c.attempt, res.status)
		if err == nil {
			slog.DebugContext(ctx, "webhook delivered", "delivery_id", c.id, "event_id", c.eventID, "attempt", c.attempt)
		}
		return err
	}

	var status *int
	if res.status != 0 {
		status = &res.status
	}
	if c.attempt >= d.maxAttempts() {
		_, err := d.db.Exec(ctx,
			`UPDATE webhook_deliveries
			 SET status = 'failed', last_response_status = $3, last_error = $4
			 WHERE id = $1 AND status = 'pending' AND attempt_count = $2`, c.id, c.attempt, status, res.reason)
		if err == nil {
			slog.ErrorContext(ctx, "webhook delivery failed for good; kept for inspection",
				"delivery_id", c.id, "event_id", c.eventID, "endpoint_id", c.endpointID, "attempts", c.attempt, "reason", res.reason)
		}
		return err
	}

	delay := d.backoff(c.attempt)
	_, err := d.db.Exec(ctx,
		`UPDATE webhook_deliveries
		 SET next_attempt_at = now() + ($5::bigint * interval '1 millisecond'), last_response_status = $3, last_error = $4
		 WHERE id = $1 AND status = 'pending' AND attempt_count = $2`,
		c.id, c.attempt, status, res.reason, delay.Milliseconds())
	if err == nil {
		slog.WarnContext(ctx, "webhook delivery attempt failed; will retry",
			"delivery_id", c.id, "event_id", c.eventID, "endpoint_id", c.endpointID,
			"attempt", c.attempt, "reason", res.reason, "retry_in", delay.Round(time.Millisecond).String())
	}
	return err
}

// backoff is the wait after attempt n failed (1-based): the next schedule entry,
// jittered by up to 20% so deliveries that failed together do not retry together.
func (d *Dispatcher) backoff(n int) time.Duration {
	base := d.schedule[n]
	return base + time.Duration(float64(base)*d.jitter())
}
