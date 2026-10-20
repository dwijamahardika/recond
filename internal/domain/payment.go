package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is a payment's externally visible status.
type Status string

// Payment statuses.
const (
	StatusPending           Status = "pending"
	StatusPaid              Status = "paid"
	StatusSettled           Status = "settled"
	StatusExpired           Status = "expired"
	StatusRefunded          Status = "refunded"
	StatusPartiallyRefunded Status = "partially_refunded"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusPaid, StatusSettled, StatusExpired, StatusRefunded, StatusPartiallyRefunded:
		return true
	}
	return false
}

// PaymentEvent is one provider status notification (a QRIS-style webhook).
// Providers deliver these at least once and in no particular order.
type PaymentEvent struct {
	EventID        string    `json:"event_id"`
	Provider       string    `json:"provider,omitempty"`
	PaymentRef     string    `json:"payment_ref"`
	Status         Status    `json:"status"`
	Amount         int64     `json:"amount"`
	RefundedAmount int64     `json:"refunded_amount,omitempty"` // cumulative, not a delta
	Currency       Currency  `json:"currency"`
	Sequence       int64     `json:"sequence,omitempty"` // per-payment provider sequence; 0 = not sent
	OccurredAt     time.Time `json:"occurred_at"`
}

// ErrInvalidEvent wraps every event validation failure.
var ErrInvalidEvent = errors.New("invalid payment event")

// Validate checks every field recond relies on.
func (e PaymentEvent) Validate() error {
	var errs []string
	if e.EventID == "" || len(e.EventID) > 128 {
		errs = append(errs, "event_id required (max 128)")
	}
	if e.Provider == "" {
		errs = append(errs, "provider required")
	}
	if e.PaymentRef == "" || len(e.PaymentRef) > 200 {
		errs = append(errs, "payment_ref required (max 200)")
	}
	if !e.Status.Valid() {
		errs = append(errs, fmt.Sprintf("unknown status %q", e.Status))
	}
	if !ValidAmount(e.Amount) || !ValidAmount(e.RefundedAmount) {
		errs = append(errs, "amounts must be 0..MaxAmount minor units")
	}
	if e.Currency != "" && !e.Currency.Valid() {
		errs = append(errs, "unknown currency")
	}
	if e.Sequence < 0 {
		errs = append(errs, "sequence must be >= 0")
	}
	if e.OccurredAt.IsZero() {
		errs = append(errs, "occurred_at required")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidEvent, strings.Join(errs, "; "))
	}
	return nil
}

// Version is an event's position in provider order: sequence first (when the
// provider sends one), then the provider's event time, then event id as a
// deterministic tie-break. Arrival order never matters.
type Version struct {
	Seq int64  `json:"seq"`
	At  int64  `json:"at"` // unix nanoseconds
	ID  string `json:"id"`
}

// Version returns the event's provider-order position.
func (e PaymentEvent) Version() Version {
	return Version{Seq: e.Sequence, At: e.OccurredAt.UnixNano(), ID: e.EventID}
}

// Less orders versions: sequence, then time, then event id.
func (v Version) Less(o Version) bool {
	if v.Seq != o.Seq {
		return v.Seq < o.Seq
	}
	if v.At != o.At {
		return v.At < o.At
	}
	return v.ID < o.ID
}

// PaymentKey identifies a payment; references are only unique per provider.
type PaymentKey struct{ Provider, Ref string }

// Payment is the state folded from a payment's events. The state is two
// monotone components - a lifecycle (pending < paid < settled, with expired a
// sink reachable only before payment) and a cumulative refunded amount - and
// the visible Status is derived from both.
type Payment struct {
	Provider       string    `json:"provider"`
	Ref            string    `json:"payment_ref"`
	Lifecycle      Status    `json:"lifecycle"` // "", pending, paid, settled or expired
	Amount         int64     `json:"amount"`
	Currency       Currency  `json:"currency"`
	RefundedAmount int64     `json:"refunded_amount"`
	FullyRefunded  bool      `json:"fully_refunded"`
	PaidAt         time.Time `json:"paid_at,omitempty"` // provider clock
	SettledAt      time.Time `json:"settled_at,omitempty"`
	Last           Version   `json:"last_version"` // greatest version seen, applied or rejected
	Events         int       `json:"events"`
	Rejected       int       `json:"rejected"`
}

// Status derives the visible status. Refund state outranks lifecycle so a
// partially refunded payment that later settles still reads as such.
func (p Payment) Status() Status {
	switch {
	case p.Lifecycle == StatusExpired:
		return StatusExpired
	case p.FullyRefunded || (p.Amount > 0 && p.RefundedAmount >= p.Amount):
		return StatusRefunded
	case p.RefundedAmount > 0:
		return StatusPartiallyRefunded
	case p.Lifecycle == "":
		return StatusPending
	}
	return p.Lifecycle
}

// Refunded returns the refunded amount in minor units, given the original
// amount (used when the provider sent "refunded" without an amount).
func (p Payment) Refunded(original int64) int64 {
	if p.Status() == StatusRefunded {
		return original
	}
	return min(p.RefundedAmount, original)
}

func lifecycleRank(s Status) int {
	switch s {
	case StatusPending:
		return 1
	case StatusPaid:
		return 2
	case StatusSettled:
		return 3
	}
	return 0
}

// Verdict is what the state machine did with one event.
type Verdict string

// Verdicts.
const (
	Applied  Verdict = "applied"
	Rejected Verdict = "rejected" // illegal in provider order: an anomaly worth surfacing
)

// Rejection records an event the transition rules refused, for the audit trail.
type Rejection struct {
	EventID string `json:"event_id"`
	Status  Status `json:"status"`
	Reason  string `json:"reason"`
}

// step applies e as the newest event in provider order.
func (p *Payment) step(e PaymentEvent) (Verdict, string) {
	switch e.Status {
	case StatusPending, StatusPaid, StatusSettled:
		if p.Lifecycle == StatusExpired {
			return Rejected, fmt.Sprintf("%s after expiry", e.Status)
		}
		if lifecycleRank(e.Status) < lifecycleRank(p.Lifecycle) {
			return Rejected, fmt.Sprintf("regression %s -> %s", p.Lifecycle, e.Status)
		}
		if e.Status == StatusPaid && p.PaidAt.IsZero() {
			p.PaidAt = e.OccurredAt
		}
		if e.Status == StatusSettled && p.SettledAt.IsZero() {
			p.SettledAt = e.OccurredAt
		}
		p.Lifecycle = e.Status
	case StatusExpired:
		switch p.Lifecycle {
		case "", StatusPending, StatusExpired:
			p.Lifecycle = StatusExpired
		default:
			return Rejected, fmt.Sprintf("expiry after %s", p.Lifecycle)
		}
	case StatusPartiallyRefunded, StatusRefunded:
		if p.Lifecycle == StatusExpired {
			return Rejected, "refund of an expired payment"
		}
		amount := p.Amount
		if amount == 0 {
			amount = e.Amount
		}
		r := e.RefundedAmount
		if e.Status == StatusRefunded && r == 0 {
			r = amount
		}
		switch {
		case e.Status == StatusPartiallyRefunded && r <= 0:
			return Rejected, "partial refund without refunded_amount"
		case r < p.RefundedAmount:
			return Rejected, fmt.Sprintf("refunded_amount went backwards %d -> %d", p.RefundedAmount, r)
		case amount > 0 && r > amount:
			return Rejected, fmt.Sprintf("refunded_amount %d exceeds amount %d", r, amount)
		}
		if lifecycleRank(p.Lifecycle) < lifecycleRank(StatusPaid) {
			p.Lifecycle = StatusPaid // a refund implies the payment happened
		}
		p.RefundedAmount = r
		if e.Status == StatusRefunded {
			p.FullyRefunded = true
		}
	default:
		return Rejected, fmt.Sprintf("unknown status %q", e.Status)
	}
	if p.Amount == 0 {
		p.Amount = e.Amount
	}
	if p.Currency == "" {
		p.Currency = e.Currency
	}
	return Applied, ""
}

func (p *Payment) record(e PaymentEvent, v Verdict) {
	if p.Provider == "" {
		p.Provider, p.Ref = e.Provider, e.PaymentRef
	}
	if ver := e.Version(); p.Events == 0 || p.Last.Less(ver) {
		p.Last = ver
	}
	p.Events++
	if v == Rejected {
		p.Rejected++
	}
}

// Observe is the incremental path. The caller has already dropped duplicate
// event ids. If e is newer than every event seen so far, applying it to the
// current state is exactly what a full replay would do, so it is applied in
// O(1) and ok is true. Otherwise ok is false and the caller must Fold the
// payment's full history plus e - the rare out-of-order path.
func (p *Payment) Observe(e PaymentEvent) (v Verdict, reason string, ok bool) {
	if p.Events > 0 && !p.Last.Less(e.Version()) {
		return "", "", false
	}
	v, reason = p.step(e)
	p.record(e, v)
	return v, reason, true
}

// Fold computes a payment's state from its events in any order, with any
// duplication: events are de-duplicated by id (first occurrence wins, as in
// storage), sorted into provider order and stepped through the transition
// rules. The result is therefore a function of the set of events alone, which
// is what makes out-of-order delivery safe and runs reproducible.
func Fold(events []PaymentEvent) (Payment, []Rejection) {
	seen := make(map[string]struct{}, len(events))
	uniq := make([]PaymentEvent, 0, len(events))
	for _, e := range events {
		if _, dup := seen[e.EventID]; dup {
			continue
		}
		seen[e.EventID] = struct{}{}
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Version().Less(uniq[j].Version()) })

	var p Payment
	var rej []Rejection
	for _, e := range uniq {
		v, reason := p.step(e)
		p.record(e, v)
		if v == Rejected {
			rej = append(rej, Rejection{EventID: e.EventID, Status: e.Status, Reason: reason})
		}
	}
	return p, rej
}
