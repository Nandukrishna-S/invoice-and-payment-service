// Package reconciler settles payments whose outcome was unknown when the pay
// call returned (timeout, dropped connection, provider error) by asking the
// provider. The provider is the only source of truth: nothing here decides an
// outcome from elapsed time, and a payment the provider cannot answer for stays
// pending and is asked about again on the next sweep.
package reconciler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"invoice-and-payment-service/internal/db"
	"invoice-and-payment-service/internal/payments"
	"invoice-and-payment-service/internal/psp"
)

const (
	defaultPageSize = 200
	defaultWorkers  = 10
)

// Prober asks the provider what happened to a charge. *psp.Client satisfies it.
type Prober interface {
	Query(ctx context.Context, paymentRef uuid.UUID) psp.Outcome
}

// ResolutionListener records a definitive provider answer. It crosses a module
// boundary: the invoice module implements it, because resolving a payment also
// settles its attempt and the invoice, in one transaction.
type ResolutionListener interface {
	ResolvePayment(ctx context.Context, paymentRef uuid.UUID, outcome psp.Outcome) error
}

type Reconciler struct {
	db       db.Querier
	payments *payments.Service
	prober   Prober
	listener ResolutionListener
	// minAge keeps the reconciler away from payments whose inline provider call
	// may still be running. It decides when to ask, never what the answer is.
	minAge   time.Duration
	pageSize int
	workers  int
}

// New builds a reconciler. minAge should be the provider call's total timeout,
// so the inline call owns a payment until its own deadline has passed.
func New(q db.Querier, prober Prober, listener ResolutionListener, minAge time.Duration) *Reconciler {
	return &Reconciler{
		db: q, payments: payments.NewService(), prober: prober, listener: listener,
		minAge: minAge, pageSize: defaultPageSize, workers: defaultWorkers,
	}
}

// Result counts what one sweep found.
type Result struct {
	Checked    int // payments the provider was asked about
	Resolved   int // definitive answers recorded
	Processing int // the provider is still working on them
	NotFound   int // the provider has no record of them
	Unknown    int // the provider could not be asked, or did not answer clearly
	Errors     int // definitive answers that could not be recorded
}

// Run sweeps immediately and then every interval until ctx is cancelled. A sweep
// never overlaps the next one. Cancelling ctx stops it, including mid-sweep.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	slog.InfoContext(ctx, "reconciler started", "interval", interval.String(), "min_age", r.minAge.String())
	defer slog.InfoContext(context.WithoutCancel(ctx), "reconciler stopped")

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "reconciler sweep failed", "error", err)
		}
		timer.Reset(interval)
	}
}

// RunOnce asks the provider about every pending payment old enough, and records
// the definitive answers. A plain SELECT is enough: the conditional update inside
// ResolvePayment makes it safe against the inline pay call and other reconcilers,
// and it keeps any lock or transaction from being held across a provider call.
func (r *Reconciler) RunOnce(ctx context.Context) (Result, error) {
	var (
		total  Result
		cursor payments.PendingCursor
	)
	for ctx.Err() == nil {
		page, err := r.payments.PendingPage(ctx, r.db, r.minAge, cursor, r.pageSize)
		if err != nil {
			return total, err
		}
		if len(page) == 0 {
			break
		}
		total.add(r.checkPage(ctx, page))
		cursor = page[len(page)-1].Cursor()
	}
	if total.Resolved > 0 {
		slog.InfoContext(ctx, "reconciled payments", "resolved", total.Resolved, "checked", total.Checked)
	}
	return total, ctx.Err()
}

func (r *Reconciler) checkPage(ctx context.Context, page []payments.PendingRef) Result {
	var (
		mu  sync.Mutex
		out Result
		wg  sync.WaitGroup
		sem = make(chan struct{}, r.workers)
	)
	for _, p := range page {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return out
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			one := r.check(ctx, p)
			mu.Lock()
			out.add(one)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (r *Reconciler) check(ctx context.Context, p payments.PendingRef) Result {
	res := Result{Checked: 1}
	outcome := r.prober.Query(ctx, p.RefID)
	switch outcome.Status {
	case psp.Succeeded, psp.Failed:
		if err := r.listener.ResolvePayment(ctx, p.RefID, outcome); err != nil {
			if ctx.Err() == nil {
				slog.ErrorContext(ctx, "could not record the provider's answer; will retry next sweep",
					"payment_ref_id", p.RefID, "error", err)
			}
			res.Errors++
			return res
		}
		res.Resolved++
	case psp.Processing:
		res.Processing++
	case psp.NotFound:
		// Possibly a crash between reserving and calling the provider. Never inferred
		// to be a failure: it is asked about again every sweep, and surfaced.
		slog.WarnContext(ctx, "provider has no record of a pending payment",
			"payment_ref_id", p.RefID, "pending_for", time.Since(p.CreatedAt).Round(time.Second).String())
		res.NotFound++
	default:
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "could not learn a pending payment's outcome; will retry next sweep",
				"payment_ref_id", p.RefID, "cause", outcome.Cause)
		}
		res.Unknown++
	}
	return res
}

func (r *Result) add(o Result) {
	r.Checked += o.Checked
	r.Resolved += o.Resolved
	r.Processing += o.Processing
	r.NotFound += o.NotFound
	r.Unknown += o.Unknown
	r.Errors += o.Errors
}
