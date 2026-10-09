package invoices

import (
	"fmt"
	"time"
)

// fiscalYear returns the year the financial year containing t starts in. The
// year runs April to March, and the boundary is judged in loc (India's local
// time), not UTC, so an invoice finalized just after midnight on 1 April IST
// belongs to the new year.
func fiscalYear(t time.Time, loc *time.Location) int {
	t = t.In(loc)
	if t.Month() >= time.April {
		return t.Year()
	}
	return t.Year() - 1
}

// formatInvoiceNumber renders e.g. INV-000042/26-27 (16 characters, matching
// CGST Rule 46(b)). The database caps the counter at 999999 and the prefix at
// 3 characters, so the result always fits.
func formatInvoiceNumber(prefix string, n int64, fiscalYear int) string {
	return fmt.Sprintf("%s-%06d/%02d-%02d", prefix, n, fiscalYear%100, (fiscalYear+1)%100)
}
