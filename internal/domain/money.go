// Package domain holds recond's vocabulary: money, civil dates, fee rules,
// ledger transactions, settlement lines, the payment state machine and the
// outcome classes a reconciliation can produce. It has no dependencies beyond
// the standard library and performs no I/O.
package domain

// Currency is an ISO 4217 alphabetic code. Every amount in recond is an int64
// count of the currency's minor units - never a float - so sums and
// comparisons are exact.
type Currency string

// exponents lists the ISO 4217 minor-unit digits for the currencies recond
// accepts. An unknown currency is rejected at the edge rather than guessed:
// mis-scaling an amount by 100 is the most expensive bug a reconciler can have.
var exponents = map[Currency]int{
	"IDR": 2, "SGD": 2, "MYR": 2, "PHP": 2, "THB": 2, "USD": 2, "EUR": 2, "AUD": 2,
	"VND": 0, "JPY": 0, "KRW": 0,
}

// Valid reports whether c is a currency recond knows the scale of.
func (c Currency) Valid() bool {
	_, ok := exponents[c]
	return ok
}

// Exponent returns the number of minor-unit digits, or -1 for an unknown currency.
func (c Currency) Exponent() int {
	if e, ok := exponents[c]; ok {
		return e
	}
	return -1
}

// MaxAmount bounds a single amount: 10^15 minor units (ten trillion IDR, or
// ten trillion dollars in cents) is far above any real payment, and 9,000 such
// amounts still sum inside int64. Sums over unbounded sets use AddChecked.
const MaxAmount int64 = 1_000_000_000_000_000

// ValidAmount accepts 0..MaxAmount. Zero is legal (a fully refunded payment
// settles nothing); negative amounts are adjustments recond does not model.
func ValidAmount(minor int64) bool { return minor >= 0 && minor <= MaxAmount }

// AddChecked returns a+b and false if the sum overflows int64.
func AddChecked(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}
