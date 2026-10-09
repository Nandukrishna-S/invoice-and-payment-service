package invoices

import (
	"math"

	"invoice-and-payment-service/internal/apperr"
)

// ComputeTotals is the single place line amounts and the invoice total are
// calculated, in integer cents with every multiplication and sum overflow-checked.
// Inputs must already be positive; a non-positive value is a caller bug and
// is rejected rather than producing a wrong total.
func ComputeTotals(lines []LineInput) ([]LineItem, int64, error) {
	items := make([]LineItem, len(lines))
	var total int64
	for i, l := range lines {
		if l.Quantity < 1 || l.UnitAmountCents < 1 {
			return nil, 0, apperr.Validation("line_items", "quantity and unit_amount_cents must be positive")
		}
		if l.Quantity > math.MaxInt64/l.UnitAmountCents {
			return nil, 0, ErrAmountOverflow
		}
		amount := l.Quantity * l.UnitAmountCents
		if total > math.MaxInt64-amount {
			return nil, 0, ErrAmountOverflow
		}
		total += amount
		items[i] = LineItem{
			Description:     l.Description,
			Quantity:        l.Quantity,
			UnitAmountCents: l.UnitAmountCents,
			AmountCents:     amount,
		}
	}
	return items, total, nil
}
