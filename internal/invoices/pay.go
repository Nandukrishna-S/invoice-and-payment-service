package invoices

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"invoice-and-payment-service/internal/apperr"
	"invoice-and-payment-service/internal/psp"
	"invoice-and-payment-service/internal/webhooks"
)

const (
	maxCardTokenLen = 100
	// settleTimeout bounds the database work that follows a provider call. The call
	// itself is bounded by the PSP client's own timeouts.
	settleTimeout = 10 * time.Second
)

// PSPClient is what the pay flow needs from the payment provider. It crosses a
// module boundary, and lets tests substitute a provider that misbehaves.
type PSPClient interface {
	Charge(ctx context.Context, paymentRef uuid.UUID, cardToken string, amountCents int64) psp.Outcome
}

// Pay attempts to pay an invoice. The flow is three steps, and no database
// transaction is open during the middle one:
//
//  1. reserve: in one transaction, lock the invoice, answer a repeated
//     idempotency key, check the invoice can be paid, and record a pending
//     payment, attempt and key. Committing this is what makes the attempt exist.
//  2. charge: call the provider with the payment's id as its idempotency key.
//  3. resolve: if the provider answered definitively, record it in a second
//     transaction. If it did not (timeout, dropped connection, 5xx), the attempt
//     stays pending for the reconciler to settle by asking the provider.
func (s *Service) Pay(ctx context.Context, businessID uuid.UUID, in PayInput) (Attempt, error) {
	if err := validatePay(in); err != nil {
		return Attempt{}, err
	}

	res, err := s.reserve(ctx, businessID, in)
	if err != nil {
		return Attempt{}, err
	}
	if res.replayed {
		return res.attempt, nil
	}

	// From here the attempt is committed and the customer may be charged, so the
	// work finishes even if the HTTP caller hangs up.
	ctx = context.WithoutCancel(ctx)

	outcome := s.psp.Charge(ctx, res.attempt.PaymentRefID, in.CardToken, in.AmountCents)
	if outcome.Definitive() {
		if err := s.ResolvePayment(ctx, res.attempt.PaymentRefID, outcome); err != nil {
			// Leave it pending: the reconciler will ask the provider again.
			slog.ErrorContext(ctx, "could not record the provider's answer; the reconciler will retry",
				"payment_ref_id", res.attempt.PaymentRefID, "attempt_id", res.attempt.ID, "error", err)
		}
	} else {
		slog.WarnContext(ctx, "provider outcome unknown; attempt stays pending",
			"payment_ref_id", res.attempt.PaymentRefID, "attempt_id", res.attempt.ID,
			"provider_status", string(outcome.Status), "cause", outcome.Cause)
	}

	readCtx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	return s.repo.getAttempt(readCtx, s.pool, businessID, res.attempt.ID)
}

type reservation struct {
	attempt  Attempt
	replayed bool
}

func (s *Service) reserve(ctx context.Context, businessID uuid.UUID, in PayInput) (reservation, error) {
	var res reservation
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inv, err := s.repo.lockForUpdate(ctx, tx, businessID, in.InvoiceID)
		if err != nil {
			return err
		}

		// A repeated key answers with the attempt it created, even if the invoice
		// has moved on since (a replayed successful payment must not become a 409).
		hash := payRequestHash(inv.ID, in.CardToken, in.AmountCents)
		stored, err := s.repo.findKey(ctx, tx, businessID, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if stored != nil {
			if !bytes.Equal(stored.hash, hash) {
				return ErrKeyReused
			}
			res.attempt, err = s.repo.getAttempt(ctx, tx, businessID, stored.attemptID)
			res.replayed = true
			return err
		}

		if !canTransition(inv.Status, StatusPaid) {
			return invalidTransition(actionPay, inv.Status)
		}
		pending, err := s.repo.hasPendingAttempt(ctx, tx, inv.ID)
		if err != nil {
			return err
		}
		if pending {
			return ErrPaymentInProgress
		}
		if in.AmountCents != inv.TotalCents {
			return ErrAmountMismatch
		}

		payment, err := s.payments.Create(ctx, tx, in.AmountCents)
		if err != nil {
			return err
		}
		attemptID := uuid.Must(uuid.NewV7())
		if err := s.repo.insertAttempt(ctx, tx, attemptID, inv.ID, payment.RefID); err != nil {
			return err
		}
		if err := s.repo.insertKey(ctx, tx, businessID, in.IdempotencyKey, hash, attemptID); err != nil {
			return err
		}
		if err := s.repo.setLatestAttempt(ctx, tx, inv.ID, attemptID); err != nil {
			return err
		}
		res.attempt, err = s.repo.getAttempt(ctx, tx, businessID, attemptID)
		return err
	})
	return res, err
}

// ResolvePayment records the provider's definitive answer for a payment: the
// payment, its attempt and, on success, the invoice, in one transaction. It is
// the single place that happens, shared by the pay flow and the reconciler,
// and it applies at most once: whoever resolves second changes nothing.
func (s *Service) ResolvePayment(ctx context.Context, paymentRef uuid.UUID, outcome psp.Outcome) error {
	if !outcome.Definitive() {
		// Refused here as well as in payments, so uncertainty can't reach any state.
		return fmt.Errorf("resolve payment %s: outcome %q is not definitive", paymentRef, outcome.Status)
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		found, err := s.repo.attemptByPaymentRef(ctx, tx, paymentRef)
		if err != nil {
			return err
		}
		// Invoice lock first, as everywhere else, so resolutions queue behind
		// other changes to the same invoice.
		inv, err := s.repo.lockByID(ctx, tx, found.InvoiceID)
		if err != nil {
			return err
		}
		// Read the attempt again now that the lock is held: another resolver may
		// have finished while we waited.
		att, err := s.repo.attemptByPaymentRef(ctx, tx, paymentRef)
		if err != nil {
			return err
		}

		applied, err := s.payments.Resolve(ctx, tx, paymentRef, outcome)
		if err != nil {
			return err
		}
		if !applied {
			if !att.Resolved() {
				// Attempts and payments are resolved together, so this means the
				// database was edited by hand. Say so rather than hide it.
				slog.ErrorContext(ctx, "payment is resolved but its attempt is still pending; needs attention",
					"payment_ref_id", paymentRef, "attempt_id", att.ID)
			}
			return nil // already resolved by the other path
		}

		if outcome.Status == psp.Failed {
			// A decline is an event, not a state: the invoice keeps its status.
			ok, err := s.repo.resolveAttempt(ctx, tx, att.ID, AttemptFailed, &outcome.Code)
			if err != nil {
				return err
			}
			if err := requireApplied(ok, att.ID); err != nil {
				return err
			}
			return s.emit(ctx, tx, webhooks.EventInvoicePaymentFailed, inv)
		}

		ok, err := s.repo.resolveAttempt(ctx, tx, att.ID, AttemptSucceeded, nil)
		if err != nil {
			return err
		}
		if err := requireApplied(ok, att.ID); err != nil {
			return err
		}
		// Only the invoice's latest attempt may mark it paid, and only from a payable state.
		if inv.LatestAttemptID == nil || *inv.LatestAttemptID != att.ID || !canTransition(inv.Status, StatusPaid) {
			slog.ErrorContext(ctx, "payment succeeded but the invoice cannot be marked paid; needs attention",
				"invoice_id", inv.ID, "attempt_id", att.ID, "invoice_status", string(inv.Status))
			return nil
		}
		if err := s.repo.markPaid(ctx, tx, inv.ID, inv.Status); err != nil {
			return err
		}
		if err := s.repo.insertTransition(ctx, tx, inv.ID, &inv.Status, StatusPaid, actionPay.reason, &att.ID); err != nil {
			return err
		}
		return s.emit(ctx, tx, webhooks.EventInvoicePaid, inv)
	})
}

func requireApplied(applied bool, attemptID uuid.UUID) error {
	if !applied {
		// The payment was pending but its attempt was not: the two never diverge
		// through this code, so something edited the database directly.
		return fmt.Errorf("attempt %s was not pending although its payment was", attemptID)
	}
	return nil
}

func (s *Service) GetAttempt(ctx context.Context, businessID, id uuid.UUID) (Attempt, error) {
	return s.repo.getAttempt(ctx, s.pool, businessID, id)
}

// ListAttempts returns an invoice's attempts, newest first.
func (s *Service) ListAttempts(ctx context.Context, businessID, invoiceID uuid.UUID) ([]Attempt, error) {
	if err := s.repo.invoiceExists(ctx, s.pool, businessID, invoiceID); err != nil {
		return nil, err
	}
	return s.repo.listAttempts(ctx, s.pool, businessID, invoiceID)
}

func validatePay(in PayInput) error {
	switch {
	case in.CardToken == "" || utf8.RuneCountInString(in.CardToken) > maxCardTokenLen:
		return apperr.Validation("card_token", "is required and must be at most 100 characters")
	case containsControl(in.CardToken):
		return apperr.Validation("card_token", "must not contain control characters")
	case in.AmountCents < 1:
		return apperr.Validation("amount_cents", "must be at least 1")
	}
	return nil
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
