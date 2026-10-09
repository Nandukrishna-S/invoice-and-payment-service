package invoices

import (
	"testing"
	"time"
)

// The expected table is written out here independently of allowedTransitions,
// straight from DESIGN.md section 2, so the test cannot pass by echoing the code.
func TestStateMachineMatchesTheDesign(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusDraft, StatusOpen}:         true,
		{StatusDraft, StatusVoid}:         true,
		{StatusOpen, StatusPaid}:          true,
		{StatusOpen, StatusVoid}:          true,
		{StatusOpen, StatusUncollectible}: true,
		{StatusUncollectible, StatusPaid}: true,
		{StatusUncollectible, StatusVoid}: true,
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			if got, want := canTransition(from, to), allowed[[2]Status{from, to}]; got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesCannotBeLeft(t *testing.T) {
	for _, terminal := range []Status{StatusPaid, StatusVoid} {
		for _, to := range allStatuses {
			if canTransition(terminal, to) {
				t.Errorf("%s must be terminal but can move to %s", terminal, to)
			}
		}
	}
}

func TestEveryActionTargetsAKnownStatus(t *testing.T) {
	for _, a := range []action{actionFinalize, actionVoid, actionMarkUncollected} {
		if !a.target.valid() || a.name == "" || a.reason == "" {
			t.Errorf("incomplete action: %+v", a)
		}
	}
}

func TestFiscalYear(t *testing.T) {
	ist := fiscalIST
	tests := []struct {
		name string
		at   time.Time
		want int
	}{
		{"mid year", time.Date(2026, 10, 9, 12, 0, 0, 0, ist), 2026},
		{"first instant of April", time.Date(2026, 4, 1, 0, 0, 0, 0, ist), 2026},
		{"last instant of March", time.Date(2027, 3, 31, 23, 59, 59, 999_000_000, ist), 2026},
		{"new year's day is still the old year", time.Date(2027, 1, 1, 0, 0, 0, 0, ist), 2026},
		{"1 April IST is already the new year while UTC is still 31 March", time.Date(2027, 3, 31, 18, 30, 0, 0, time.UTC), 2027},
		{"one second before that", time.Date(2027, 3, 31, 18, 29, 59, 0, time.UTC), 2026},
		{"a UTC instant in the previous calendar year is next year in IST", time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC), 2026},
	}
	for _, tt := range tests {
		if got := fiscalYear(tt.at, ist); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestFormatInvoiceNumber(t *testing.T) {
	tests := []struct {
		prefix string
		n      int64
		fy     int
		want   string
	}{
		{"INV", 42, 2026, "INV-000042/26-27"},
		{"INV", 1, 2026, "INV-000001/26-27"},
		{"INV", 999999, 2026, "INV-999999/26-27"},
		{"INV", 7, 2099, "INV-000007/99-00"},
		{"A", 7, 2009, "A-000007/09-10"},
	}
	for _, tt := range tests {
		got := formatInvoiceNumber(tt.prefix, tt.n, tt.fy)
		if got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
		if len(got) > 16 {
			t.Errorf("%q is %d characters, the limit is 16", got, len(got))
		}
	}
}
