package mockpsp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"invoice-and-payment-service/internal/db"
)

const settleTimeout = 5 * time.Second

type Service struct {
	db        db.Querier
	repo      repo
	delay     time.Duration // how long tok_timeout charges stay processing
	fastDelay time.Duration // how long ordinary charges take to answer

	// bg outlives any single request: a slow charge keeps settling after the
	// caller that started it has disconnected.
	bg context.Context
	wg sync.WaitGroup
}

// NewService returns a service whose background settling stops when bg is
// cancelled. Call Close after cancelling to wait for it. fastDelay is how long
// success and decline answers take; tests pass 0.
func NewService(q db.Querier, bg context.Context, processingDelay, fastDelay time.Duration) *Service {
	return &Service{db: q, delay: processingDelay, fastDelay: fastDelay, bg: bg}
}

// Close waits for background settling to stop.
func (s *Service) Close() { s.wg.Wait() }

type Result struct {
	Charge Charge
	// DropConnection tells the handler to hang up without answering.
	DropConnection bool
}

// Charge creates the charge for key, or returns the one already made with it.
func (s *Service) Charge(ctx context.Context, key, token string, amountCents int64) (Result, error) {
	b := behaviourFor(token)
	status := b.status
	if b.slow {
		status = StatusProcessing
	}
	c := Charge{IdempotencyKey: key, PSPRefID: newPSPRefID(), Token: token, AmountCents: amountCents, Status: status}
	if status == StatusFailed {
		c.FailureCode = &b.failureCode
	}

	stored, created, err := s.repo.insertIfAbsent(ctx, s.db, c)
	if err != nil {
		return Result{}, err
	}
	if !created {
		// Same key: only the identical request gets the original answer back.
		if stored.Token != token || stored.AmountCents != amountCents {
			return Result{}, ErrIdempotencyConflict
		}
		stored, err = s.settleIfDue(ctx, stored)
		return Result{Charge: stored}, err
	}

	if b.slow {
		// Settle in the background, and hold this response until it does (or until
		// the caller gives up, which does not stop the settling).
		select {
		case <-s.settleLater(key):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		if stored, err = s.repo.get(ctx, s.db, key); err != nil {
			return Result{}, err
		}
	} else if !b.dropConnection {
		// Ordinary charges take a moment, like a real provider. The charge is already
		// stored, so a caller that gives up here still gets charged, as in real life.
		if err := sleep(ctx, s.fastDelay); err != nil {
			return Result{}, err
		}
	}
	return Result{Charge: stored, DropConnection: b.dropConnection}, nil
}

// Get returns the charge made with key. A charge left processing past its delay
// (e.g. the PSP restarted mid-wait) is settled on the way out.
func (s *Service) Get(ctx context.Context, key string) (Charge, error) {
	c, err := s.repo.get(ctx, s.db, key)
	if err != nil {
		return Charge{}, err
	}
	return s.settleIfDue(ctx, c)
}

func (s *Service) settleIfDue(ctx context.Context, c Charge) (Charge, error) {
	if c.Status != StatusProcessing {
		return c, nil
	}
	if err := s.repo.settle(ctx, s.db, c.IdempotencyKey, s.delay); err != nil {
		return Charge{}, err
	}
	return s.repo.get(ctx, s.db, c.IdempotencyKey)
}

// settleLater settles the charge after the processing delay, independent of any
// request. The returned channel closes when that is done or abandoned.
func (s *Service) settleLater(key string) <-chan struct{} {
	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.bg.Done():
			return // left processing; Get settles it lazily once it is old enough
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.bg), settleTimeout)
		defer cancel()
		if err := s.repo.settle(ctx, s.db, key, 0); err != nil {
			slog.Error("mockpsp: settle failed", "idempotency_key", key, "error", err)
		}
	}()
	return done
}

// sleep waits for d, or returns early with the context's error.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
