package invoices

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"invoice-and-payment-service/internal/customers"
	"invoice-and-payment-service/internal/db"
)

type repo struct{}

const (
	invoiceColumns = `id, business_id, customer_id, status, currency, total_cents, due_date,
		invoice_sequence_number, paid_at, created_at, updated_at`

	pgForeignKeyViolation = "23503"
	customerFKConstraint  = "invoices_customer_fk"
)

func scanInvoice(row pgx.Row) (Invoice, error) {
	var inv Invoice
	err := row.Scan(&inv.ID, &inv.BusinessID, &inv.CustomerID, &inv.Status, &inv.Currency,
		&inv.TotalCents, &inv.DueDate, &inv.SequenceNumber, &inv.PaidAt, &inv.CreatedAt, &inv.UpdatedAt)
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
func (repo) insertTransition(ctx context.Context, q db.Querier, invoiceID uuid.UUID, from *Status, to Status, reason string) error {
	_, err := q.Exec(ctx,
		`INSERT INTO invoice_transitions (id, invoice_id, from_status, to_status, reason)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.Must(uuid.NewV7()), invoiceID, from, to, reason)
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
