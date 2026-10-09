package invoices

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"invoice-and-payment-service/internal/customers"
	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const (
	invoiceColumns = `id, business_id, customer_id, status, currency, total_cents, due_date,
		invoice_sequence_number, paid_at, created_at, updated_at, latest_payment_attempt_id`

	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	customerFKConstraint  = "invoices_customer_fk"
	counterRangeCheck     = "invoice_number_counter_range"
)

func scanInvoice(row pgx.Row) (Invoice, error) {
	var inv Invoice
	err := row.Scan(&inv.ID, &inv.BusinessID, &inv.CustomerID, &inv.Status, &inv.Currency,
		&inv.TotalCents, &inv.DueDate, &inv.SequenceNumber, &inv.PaidAt, &inv.CreatedAt, &inv.UpdatedAt, &inv.LatestAttemptID)
	return inv, err
}

// insert writes the invoice header and fills in its database-assigned timestamps.
// A customer that doesn't exist for this business violates the composite foreign
// key, which is reported as customer_not_found.
func (repo) insert(ctx context.Context, q db.Querier, inv *Invoice) error {
	err := q.QueryRow(ctx,
		`INSERT INTO invoices (id, business_id, customer_id, status, currency, total_cents, due_date)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at, updated_at`,
		inv.ID, inv.BusinessID, inv.CustomerID, inv.Status, inv.Currency, inv.TotalCents, inv.DueDate,
	).Scan(&inv.CreatedAt, &inv.UpdatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation && pgErr.ConstraintName == customerFKConstraint {
		return customers.ErrNotFound
	}
	return err
}

// insertLineItems writes all lines in one round trip via parallel arrays.
func (repo) insertLineItems(ctx context.Context, q db.Querier, invoiceID uuid.UUID, items []LineItem) error {
	positions := make([]int32, len(items))
	descriptions := make([]string, len(items))
	quantities := make([]int64, len(items))
	units := make([]int64, len(items))
	amounts := make([]int64, len(items))
	for i, l := range items {
		positions[i] = int32(i + 1)
		descriptions[i], quantities[i], units[i], amounts[i] = l.Description, l.Quantity, l.UnitAmountCents, l.AmountCents
	}
	_, err := q.Exec(ctx,
		`INSERT INTO invoice_line_items (invoice_id, position, description, quantity, unit_amount_cents, amount_cents)
		 SELECT $1, * FROM unnest($2::int[], $3::text[], $4::bigint[], $5::bigint[], $6::bigint[])`,
		invoiceID, positions, descriptions, quantities, units, amounts)
	return err
}

// insertTransition appends to the status history. from is nil for creation.
func (repo) insertTransition(ctx context.Context, q db.Querier, invoiceID uuid.UUID, from *Status, to Status, reason string, attemptID *uuid.UUID) error {
	_, err := q.Exec(ctx,
		`INSERT INTO invoice_transitions (id, invoice_id, from_status, to_status, reason, attempt_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.Must(uuid.NewV7()), invoiceID, from, to, reason, attemptID)
	return err
}

func (r repo) get(ctx context.Context, q db.Querier, businessID, id uuid.UUID) (Invoice, error) {
	inv, err := scanInvoice(q.QueryRow(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE business_id = $1 AND id = $2`, businessID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	items, err := r.lineItems(ctx, q, businessID, []uuid.UUID{id})
	if err != nil {
		return Invoice{}, err
	}
	inv.LineItems = items[id]
	return inv, nil
}

// list returns up to limit invoices, newest first, optionally filtered by status.
// The id is the keyset cursor (UUIDv7 sorts by creation time) and is only a
// position, never looked up.
func (r repo) list(ctx context.Context, q db.Querier, businessID uuid.UUID, status *Status, after *uuid.UUID, limit int) ([]Invoice, error) {
	// Only fixed fragments are concatenated; every value is a bind parameter.
	var where strings.Builder
	where.WriteString("business_id = $1")
	args := []any{businessID}
	if status != nil {
		args = append(args, *status)
		fmt.Fprintf(&where, " AND status = $%d", len(args))
	}
	if after != nil {
		args = append(args, *after)
		fmt.Fprintf(&where, " AND id < $%d", len(args))
	}
	args = append(args, limit)

	rows, err := q.Query(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE `+where.String()+
			fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invs []Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		invs = append(invs, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(invs) == 0 {
		return nil, nil
	}

	ids := make([]uuid.UUID, len(invs))
	for i, inv := range invs {
		ids[i] = inv.ID
	}
	items, err := r.lineItems(ctx, q, businessID, ids)
	if err != nil {
		return nil, err
	}
	for i := range invs {
		invs[i].LineItems = items[invs[i].ID]
	}
	return invs, nil
}

// lineItems loads the lines of several invoices at once. It joins invoices and
// filters on business_id, since line items carry no tenant column of their own.
func (repo) lineItems(ctx context.Context, q db.Querier, businessID uuid.UUID, invoiceIDs []uuid.UUID) (map[uuid.UUID][]LineItem, error) {
	rows, err := q.Query(ctx,
		`SELECT li.invoice_id, li.description, li.quantity, li.unit_amount_cents, li.amount_cents
		 FROM invoice_line_items li
		 JOIN invoices i ON i.id = li.invoice_id
		 WHERE i.business_id = $1 AND li.invoice_id = ANY($2)
		 ORDER BY li.invoice_id, li.position`, businessID, invoiceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]LineItem, len(invoiceIDs))
	for rows.Next() {
		var (
			id uuid.UUID
			l  LineItem
		)
		if err := rows.Scan(&id, &l.Description, &l.Quantity, &l.UnitAmountCents, &l.AmountCents); err != nil {
			return nil, err
		}
		out[id] = append(out[id], l)
	}
	return out, rows.Err()
}

// lockForUpdate reads the invoice header and holds its row lock until the
// transaction ends, so every status change on one invoice is serialised.
func (repo) lockForUpdate(ctx context.Context, q db.Querier, businessID, id uuid.UUID) (Invoice, error) {
	inv, err := scanInvoice(q.QueryRow(ctx,
		`SELECT `+invoiceColumns+` FROM invoices WHERE business_id = $1 AND id = $2 FOR UPDATE`, businessID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	return inv, err
}

// updateStatus moves the invoice from one status to another, optionally
// recording its sequence number. The WHERE on the old status means a change
// can only apply to the state the caller actually checked.
func (repo) updateStatus(ctx context.Context, q db.Querier, id uuid.UUID, from, to Status, sequenceNumber *string) (time.Time, error) {
	var updatedAt time.Time
	err := q.QueryRow(ctx,
		`UPDATE invoices
		 SET status = $3, invoice_sequence_number = COALESCE($4, invoice_sequence_number), updated_at = now()
		 WHERE id = $1 AND status = $2
		 RETURNING updated_at`, id, from, to, sequenceNumber).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, errStaleStatus
	}
	return updatedAt, err
}

// nextSequenceNumber takes the next counter value for the business and
// financial year, creating the counter on first use. The row lock it takes is
// held until the transaction ends, so a rollback leaves no gap.
func (repo) nextSequenceNumber(ctx context.Context, q db.Querier, businessID uuid.UUID, fiscalYear int) (prefix string, n int64, err error) {
	err = q.QueryRow(ctx,
		`INSERT INTO invoice_number_sequences (business_id, fiscal_year, last_value)
		 VALUES ($1, $2, 1)
		 ON CONFLICT (business_id, fiscal_year)
		 DO UPDATE SET last_value = invoice_number_sequences.last_value + 1
		 RETURNING prefix, last_value`, businessID, fiscalYear).Scan(&prefix, &n)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation && pgErr.ConstraintName == counterRangeCheck {
		return "", 0, errNumberExhausted
	}
	return prefix, n, err
}

// ---- payment attempts -------------------------------------------------------

const (
	pgUniqueViolation = "23505"
	onePendingIndex   = "invoice_payment_attempts_one_pending"
	idempotencyKeyPK  = "invoice_payment_idempotency_keys_pkey"
	attemptColumns    = `a.id, a.invoice_id, a.payment_ref_id, p.amount_cents, a.status, a.failure_code, p.psp_ref_id, a.created_at, a.resolved_at`
	attemptFromTenant = `FROM invoice_payment_attempts a
		JOIN payments p ON p.payment_ref_id = a.payment_ref_id
		JOIN invoices i ON i.id = a.invoice_id`
)

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.InvoiceID, &a.PaymentRefID, &a.AmountCents, &a.Status, &a.FailureCode, &a.PSPRefID, &a.CreatedAt, &a.ResolvedAt)
	return a, err
}

// getAttempt reads one attempt, but only if its invoice belongs to the business.
func (repo) getAttempt(ctx context.Context, q db.Querier, businessID, id uuid.UUID) (Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx,
		`SELECT `+attemptColumns+` `+attemptFromTenant+` WHERE i.business_id = $1 AND a.id = $2`, businessID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	return a, err
}

// attemptByPaymentRef is for internal resolution paths (the pay flow and the
// reconciler), which start from a payment and have no business context. The
// ref comes from our own tables, never from a request.
func (repo) attemptByPaymentRef(ctx context.Context, q db.Querier, ref uuid.UUID) (Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx,
		`SELECT `+attemptColumns+` `+attemptFromTenant+` WHERE a.payment_ref_id = $1`, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	return a, err
}

// listAttempts returns an invoice's attempts newest first (UUIDv7 ids sort by time).
func (repo) listAttempts(ctx context.Context, q db.Querier, businessID, invoiceID uuid.UUID) ([]Attempt, error) {
	rows, err := q.Query(ctx,
		`SELECT `+attemptColumns+` `+attemptFromTenant+`
		 WHERE i.business_id = $1 AND a.invoice_id = $2 ORDER BY a.id DESC`, businessID, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (repo) invoiceExists(ctx context.Context, q db.Querier, businessID, id uuid.UUID) error {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM invoices WHERE business_id = $1 AND id = $2`, businessID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (repo) hasPendingAttempt(ctx context.Context, q db.Querier, invoiceID uuid.UUID) (bool, error) {
	var pending bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM invoice_payment_attempts WHERE invoice_id = $1 AND status = 'pending')`,
		invoiceID).Scan(&pending)
	return pending, err
}

// insertAttempt records a pending attempt. If the invoice already has one, the
// partial unique index rejects it, which is reported as payment_in_progress.
func (repo) insertAttempt(ctx context.Context, q db.Querier, id, invoiceID, paymentRef uuid.UUID) error {
	_, err := q.Exec(ctx,
		`INSERT INTO invoice_payment_attempts (id, invoice_id, payment_ref_id) VALUES ($1, $2, $3)`,
		id, invoiceID, paymentRef)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == onePendingIndex {
		return ErrPaymentInProgress
	}
	return err
}

func (repo) setLatestAttempt(ctx context.Context, q db.Querier, invoiceID, attemptID uuid.UUID) error {
	_, err := q.Exec(ctx,
		`UPDATE invoices SET latest_payment_attempt_id = $2, updated_at = now() WHERE id = $1`, invoiceID, attemptID)
	return err
}

type storedKey struct {
	hash      []byte
	attemptID uuid.UUID
}

func (repo) findKey(ctx context.Context, q db.Querier, businessID uuid.UUID, key string) (*storedKey, error) {
	var k storedKey
	err := q.QueryRow(ctx,
		`SELECT request_hash, attempt_id FROM invoice_payment_idempotency_keys WHERE business_id = $1 AND key = $2`,
		businessID, key).Scan(&k.hash, &k.attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &k, err
}

// insertKey stores the key. Two requests with the same key for different
// invoices don't share an invoice lock, so the primary key is what stops the
// second one; it is reported as the key having been used with another request.
func (repo) insertKey(ctx context.Context, q db.Querier, businessID uuid.UUID, key string, hash []byte, attemptID uuid.UUID) error {
	_, err := q.Exec(ctx,
		`INSERT INTO invoice_payment_idempotency_keys (business_id, key, request_hash, attempt_id) VALUES ($1, $2, $3, $4)`,
		businessID, key, hash, attemptID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == idempotencyKeyPK {
		return ErrKeyReused
	}
	return err
}

// resolveAttempt records the outcome on an attempt that is still pending and
// reports whether it did.
func (repo) resolveAttempt(ctx context.Context, q db.Querier, id uuid.UUID, status AttemptStatus, failureCode *string) (bool, error) {
	tag, err := q.Exec(ctx,
		`UPDATE invoice_payment_attempts SET status = $2, failure_code = $3, resolved_at = now()
		 WHERE id = $1 AND status = 'pending'`, id, status, failureCode)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// lockByID locks an invoice from an internal resolution path that starts from
// an attempt, so the id is ours and not a request's.
func (repo) lockByID(ctx context.Context, q db.Querier, id uuid.UUID) (Invoice, error) {
	inv, err := scanInvoice(q.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	return inv, err
}

// markPaid moves the invoice to paid, guarded on the status the caller checked.
func (repo) markPaid(ctx context.Context, q db.Querier, id uuid.UUID, from Status) error {
	tag, err := q.Exec(ctx,
		`UPDATE invoices SET status = 'paid', paid_at = now(), updated_at = now() WHERE id = $1 AND status = $2`, id, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errStaleStatus
	}
	return nil
}
