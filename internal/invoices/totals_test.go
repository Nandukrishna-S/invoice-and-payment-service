package invoices

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestComputeTotals(t *testing.T) {
	tests := []struct {
		name      string
		lines     []LineInput
		wantTotal int64
		wantErr   error
	}{
		{"single line", []LineInput{{"a", 3, 1500}}, 4500, nil},
		{"several lines", []LineInput{{"a", 2, 100}, {"b", 1, 50}, {"c", 10, 7}}, 320, nil},
		{"largest representable amount", []LineInput{{"a", 1, math.MaxInt64}}, math.MaxInt64, nil},
		{"product exactly at the limit", []LineInput{{"a", 2, math.MaxInt64 / 2}}, math.MaxInt64 - 1, nil},
		{"product just over the limit", []LineInput{{"a", 2, math.MaxInt64/2 + 1}}, 0, ErrAmountOverflow},
		{"quantity times unit overflows", []LineInput{{"a", math.MaxInt64, 2}}, 0, ErrAmountOverflow},
		{"both factors huge", []LineInput{{"a", math.MaxInt64, math.MaxInt64}}, 0, ErrAmountOverflow},
		{"sum overflows though each line fits", []LineInput{{"a", 1, math.MaxInt64}, {"b", 1, 1}}, 0, ErrAmountOverflow},
		{"sum of two large lines overflows", []LineInput{{"a", 1, math.MaxInt64/2 + 1}, {"b", 1, math.MaxInt64/2 + 1}}, 0, ErrAmountOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, total, err := ComputeTotals(tt.lines)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if total != tt.wantTotal {
				t.Fatalf("total = %d, want %d", total, tt.wantTotal)
			}
			var sum int64
			for i, it := range items {
				if it.AmountCents != tt.lines[i].Quantity*tt.lines[i].UnitAmountCents {
					t.Fatalf("line %d amount = %d", i, it.AmountCents)
				}
				sum += it.AmountCents
			}
			if sum != total {
				t.Fatalf("lines sum to %d but total is %d", sum, total)
			}
		})
	}
}

func TestComputeTotalsRejectsNonPositiveInputs(t *testing.T) {
	for _, l := range []LineInput{{"a", 0, 10}, {"a", 10, 0}, {"a", -1, 10}, {"a", 10, -1}} {
		if _, _, err := ComputeTotals([]LineInput{l}); err == nil {
			t.Errorf("%+v should be rejected", l)
		}
	}
}

// Cross-check against arbitrary-precision arithmetic: whenever the exact result
// fits in int64 we must return it, and whenever it doesn't we must say overflow.
func TestComputeTotalsAgreesWithBigInt(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pick := func() int64 {
		switch rng.Intn(4) {
		case 0:
			return rng.Int63n(1000) + 1
		case 1:
			return rng.Int63n(1<<31) + 1
		case 2:
			return math.MaxInt64 - rng.Int63n(1000)
		default:
			return rng.Int63n(math.MaxInt64) + 1
		}
	}
	for n := 0; n < 20000; n++ {
		lines := make([]LineInput, rng.Intn(4)+1)
		exact := new(big.Int)
		for i := range lines {
			lines[i] = LineInput{"x", pick(), pick()}
			exact.Add(exact, new(big.Int).Mul(big.NewInt(lines[i].Quantity), big.NewInt(lines[i].UnitAmountCents)))
		}
		_, total, err := ComputeTotals(lines)
		if exact.IsInt64() {
			if err != nil || total != exact.Int64() {
				t.Fatalf("%+v: got %d, %v; want %s", lines, total, err, exact)
			}
		} else if !errors.Is(err, ErrAmountOverflow) {
			t.Fatalf("%+v: exact total %s does not fit, got %d, %v", lines, exact, total, err)
		}
	}
}
