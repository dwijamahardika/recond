package domain

import (
	"errors"
	"fmt"
	"math/bits"
)

// Rounding is how a provider rounds the percentage part of a fee to a whole
// minor unit. Providers differ, which is why fee comparisons carry a tolerance.
type Rounding string

// Rounding modes.
const (
	RoundHalfUp   Rounding = "half_up"
	RoundHalfEven Rounding = "half_even"
	RoundDown     Rounding = "down"
	RoundUp       Rounding = "up"
)

// FeeRule is a merchant discount rate (MDR) in basis points of the gross
// amount plus a fixed component in minor units:
//
//	fee = round(gross * RateBPS / 10000) + FixedMinor, capped at gross
//
// Values in this repository are illustrative examples for fictional
// providers, not any real provider's pricing.
type FeeRule struct {
	RateBPS    int64    `json:"rate_bps"`
	FixedMinor int64    `json:"fixed_minor"`
	Rounding   Rounding `json:"rounding,omitempty"`
}

// Validate reports every problem with the rule.
func (r FeeRule) Validate() error {
	var errs []error
	if r.RateBPS < 0 || r.RateBPS > 10_000 {
		errs = append(errs, fmt.Errorf("rate_bps %d outside 0..10000", r.RateBPS))
	}
	if r.FixedMinor < 0 || r.FixedMinor > MaxAmount {
		errs = append(errs, fmt.Errorf("fixed_minor %d out of range", r.FixedMinor))
	}
	switch r.Rounding {
	case "", RoundHalfUp, RoundHalfEven, RoundDown, RoundUp:
	default:
		errs = append(errs, fmt.Errorf("unknown rounding %q", r.Rounding))
	}
	return errors.Join(errs...)
}

// Fee computes the expected fee for a gross amount. The multiplication is done
// in 128 bits so no gross within MaxAmount can overflow, whatever the rate.
func (r FeeRule) Fee(gross int64) int64 {
	if gross <= 0 {
		return 0
	}
	bps := min(max(r.RateBPS, 0), 10_000)            // Validate rejects these; never panic in Div64
	hi, lo := bits.Mul64(uint64(gross), uint64(bps)) //nolint:gosec // both checked positive above
	q, rem := bits.Div64(hi, lo, 10_000)             // hi < 5_000 since gross < 2^63 and bps <= 10^4
	switch r.Rounding {
	case RoundDown:
	case RoundUp:
		if rem > 0 {
			q++
		}
	case RoundHalfEven:
		if rem*2 > 10_000 || (rem*2 == 10_000 && q%2 == 1) {
			q++
		}
	default: // half_up
		if rem*2 >= 10_000 {
			q++
		}
	}
	fee, ok := AddChecked(int64(q), r.FixedMinor) //nolint:gosec // q <= gross < 2^63
	if !ok || fee > gross {
		fee = gross
	}
	return fee
}
