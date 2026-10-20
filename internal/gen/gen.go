// Package gen produces synthetic, obviously fake reconciliation data - a
// ledger, provider settlement files and a webhook stream - and injects faults
// with ground truth, so the engine can be tested against known answers and
// benchmarked at scale.
//
// Every injected fault is identifiable in principle: the generator refuses to
// inject a fault that no reconciler could attribute (for example, dropping one
// of two equal-amount transactions from an unitemised payout). That is what
// lets the ground-truth test demand exact classification instead of a score.
package gen

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

// Item-level faults (itemised provider psp_a).
const (
	FaultDropSettlement   = "drop_settlement"   // provider never settled it
	FaultDropLedger       = "drop_ledger"       // settled, but our ledger never recorded it
	FaultDuplicate        = "duplicate"         // settled twice under different settlement ids
	FaultAmountChange     = "amount_change"     // settled gross differs
	FaultFeeChange        = "fee_change"        // fee differs from the agreed rule
	FaultDelay            = "delay"             // settled after T+N
	FaultCorruptReference = "corrupt_reference" // reference empty/mangled: must still match by tolerance
)

// Batch-level faults (payout provider bank_b).
const (
	FaultBatchDrop       = "batch_drop"        // one transaction left out of an unitemised payout
	FaultBatchLedgerDrop = "batch_ledger_drop" // payout includes a transaction our ledger lacks
	FaultBatchAmount     = "batch_amount"      // payout credit altered
	FaultBatchFee        = "batch_fee"         // charges altered (gross unchanged)
	FaultBatchDuplicate  = "batch_duplicate"   // payout credited twice
	FaultBatchDelay      = "batch_delay"       // payout after T+N
	FaultItemDelay       = "item_delay"        // one transaction missed the payout, credited alone after T+N
)

// Fault kinds by provider type.
var (
	ItemFaults  = []string{FaultDropSettlement, FaultDropLedger, FaultDuplicate, FaultAmountChange, FaultFeeChange, FaultDelay, FaultCorruptReference}
	BatchFaults = []string{FaultBatchDrop, FaultBatchLedgerDrop, FaultBatchAmount, FaultBatchFee, FaultBatchDuplicate, FaultBatchDelay, FaultItemDelay}
)

// Config controls a generated dataset. Zero values take the defaults noted.
type Config struct {
	Seed     int64
	Payments int       // payment attempts (default 1000)
	Start    time.Time // first day, UTC midnight (default 2026-10-01)
	Days     int       // default 3
	Rules    *match.Config

	ShareB            float64 // fraction through bank_b (default 0.4)
	ExpireRate        float64 // attempts that expire unpaid (default 0.05)
	PartialRefundRate float64 // default 0.03
	FullRefundRate    float64 // default 0.01
	FaultRate         float64 // psp_a transactions given an item fault (default 0.03; <0 disables)
	BatchFaultRate    float64 // bank_b payouts given a batch fault (default 0.5; <0 disables)
	DupEventRate      float64 // webhook re-deliveries (default 0.15)
	BoundaryRate      float64 // payments placed just before a cut-off boundary (default 0.02)
	// StaleResendRate: bank_b (no sequence numbers) re-sends a stale
	// "pending" with a fresh event id and a later timestamp after payment.
	// Only the transition rules - not event ordering - can reject it.
	StaleResendRate float64 // default 0.03
	CycleHours      int     // bank_b payout cycle (default 6)
	Faults          map[string]bool
}

func (c *Config) defaults() error {
	if c.Payments <= 0 {
		c.Payments = 1000
	}
	if c.Start.IsZero() {
		c.Start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	}
	if c.Days <= 0 {
		c.Days = 3
	}
	def := func(v *float64, d float64) {
		switch {
		case *v == 0:
			*v = d
		case *v < 0:
			*v = 0
		}
	}
	def(&c.ShareB, 0.4)
	def(&c.ExpireRate, 0.05)
	def(&c.PartialRefundRate, 0.03)
	def(&c.FullRefundRate, 0.01)
	def(&c.FaultRate, 0.03)
	def(&c.BatchFaultRate, 0.5)
	def(&c.DupEventRate, 0.15)
	def(&c.BoundaryRate, 0.02)
	def(&c.StaleResendRate, 0.03)
	if c.CycleHours <= 0 || 24%c.CycleHours != 0 {
		c.CycleHours = 6
	}
	if c.Rules == nil {
		return fmt.Errorf("gen: rules required")
	}
	for _, p := range []string{ProviderA, ProviderB} {
		if _, ok := c.Rules.Provider(p); !ok {
			return fmt.Errorf("gen: rules must define %s", p)
		}
	}
	if len(c.Faults) == 0 {
		c.Faults = map[string]bool{}
		for _, f := range append(append([]string(nil), ItemFaults...), BatchFaults...) {
			c.Faults[f] = true
		}
	}
	return nil
}

// The fictional providers and currency of generated worlds.
const (
	ProviderA = "psp_a"
	ProviderB = "bank_b"
	Currency  = domain.Currency("IDR")
)

// Fault is one injected discrepancy and the verdict a correct reconciler
// must reach. Key is the record that verdict attaches to: the ledger
// transaction when the settlement side is absent, otherwise the settlement
// line (for unitemised payouts, the payout line).
type Fault struct {
	Kind     string       `json:"kind"`
	Class    domain.Class `json:"class"`
	Provider string       `json:"provider"`
	Key      string       `json:"key"`
	LedgerID string       `json:"ledger_id,omitempty"`
	LineID   string       `json:"line_id,omitempty"`
	Detail   string       `json:"detail,omitempty"`
}

// Truth is what the generator knows that a reconciler must work out.
type Truth struct {
	Seed     int64                `json:"seed"`
	Payments int                  `json:"payments"`
	AsOf     time.Time            `json:"as_of"`
	Faults   []Fault              `json:"faults"`
	Expected map[domain.Class]int `json:"expected_discrepancies"`
	// Settles maps each ledger transaction to the line that settles it
	// (itemised line or payout), where one exists. Not serialised.
	Settles map[string]string `json:"-"`
	// FinalStatus maps provider/ref to the status every webhook ordering must
	// fold to. Not serialised.
	FinalStatus map[domain.PaymentKey]domain.Status `json:"-"`
}

// Dataset is a generated world.
type Dataset struct {
	Rules  *match.Config
	Ledger []domain.LedgerTxn
	PSPA   map[domain.Date][]PSPAItem   // by settlement date (one file each)
	BankB  map[domain.Date][]BankBEntry // by value date (one statement each)
	Events []domain.PaymentEvent        // delivery order: duplicated, out of order
	Truth  Truth
}

// PSPAItem and BankBEntry are settlement rows plus the line ID a reader will
// assign them.
type PSPAItem struct {
	ID string
	SettlementRow
}

// SettlementRow is one generated psp_a row.
type SettlementRow struct {
	SettlementID string
	Ref          string
	TxnTime      time.Time
	Date         domain.Date
	Gross, Fee   int64
}

// BankBEntry is one generated bank_b statement row.
type BankBEntry struct {
	ID          string
	ValueDate   domain.Date
	TxnTime     time.Time
	RefNo       string
	Description string
	Credit      int64
	Charges     int64
}

type payment struct {
	idx      int
	provider string
	ref      string
	amount   int64
	paidAt   time.Time
	expired  bool
	refunded int64
	full     bool
	ledger   *domain.LedgerTxn // nil when expired or dropped from the ledger
	events   []domain.PaymentEvent
	exp      match.Amounts // expected settlement given refunds
	settled  domain.Date   // 0 if never settled
}

type generator struct {
	cfg  Config
	rng  *rand.Rand
	ds   *Dataset
	pays []*payment
	ra   *match.ProviderRules
	rb   *match.ProviderRules
	// identifiability guard for the tolerance pass (see guard.go)
	openLines map[int64][]time.Time
	openTxns  map[int64][]time.Time
	nextSID   int
}

// Generate builds a dataset. The same Config always yields the same dataset.
func Generate(cfg Config) (*Dataset, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	g := &generator{
		cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)),
		ds: &Dataset{Rules: cfg.Rules, PSPA: map[domain.Date][]PSPAItem{}, BankB: map[domain.Date][]BankBEntry{},
			Truth: Truth{Seed: cfg.Seed, Payments: cfg.Payments, Expected: map[domain.Class]int{},
				Settles: map[string]string{}, FinalStatus: map[domain.PaymentKey]domain.Status{}}},
		openLines: map[int64][]time.Time{}, openTxns: map[int64][]time.Time{},
	}
	g.ra, _ = cfg.Rules.Provider(ProviderA)
	g.rb, _ = cfg.Rules.Provider(ProviderB)

	g.payments()
	g.settlePSPA()
	g.settleBankB()
	g.finish()
	return g.ds, nil
}

func (g *generator) uuid() string {
	a, b := g.rng.Uint64(), g.rng.Uint64()
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", a>>32, (a>>16)&0xffff, a&0xfff, (b>>48)&0x3fff|0x8000, b&0xffffffffffff)
}

func (g *generator) eventID() string { return fmt.Sprintf("evt_%016x", g.rng.Uint64()) }

func (g *generator) between(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(g.rng.Int63n(int64(hi-lo)))
}

// payments creates attempts, the ledger and the non-settlement webhooks.
func (g *generator) payments() {
	cfg := g.cfg
	span := time.Duration(cfg.Days) * 24 * time.Hour
	clearing := map[string]string{ProviderA: g.uuid(), ProviderB: g.uuid()}
	wallet := g.uuid()
	for i := 0; i < cfg.Payments; i++ {
		p := &payment{idx: i, provider: ProviderA}
		if g.rng.Float64() < cfg.ShareB {
			p.provider = ProviderB
		}
		p.paidAt = cfg.Start.Add(time.Duration(g.rng.Int63n(int64(span)))).Truncate(time.Millisecond)
		if g.rng.Float64() < cfg.BoundaryRate {
			p.paidAt = g.nearBoundary(p.provider)
		}
		// Log-uniform 10,000 .. 5,000,000 IDR, whole rupiah.
		major := int64(math.Exp(math.Log(1e4) + g.rng.Float64()*(math.Log(5e6)-math.Log(1e4))))
		p.amount = major * 100
		p.ref = fmt.Sprintf("ORD-%s-%07d", p.paidAt.Format("20060102"), i)
		g.pays = append(g.pays, p)

		seq := int64(0)
		ev := func(s domain.Status, at time.Time, refunded int64) {
			e := domain.PaymentEvent{EventID: g.eventID(), Provider: p.provider, PaymentRef: p.ref, Status: s,
				Amount: p.amount, RefundedAmount: refunded, Currency: Currency, OccurredAt: at}
			if p.provider == ProviderA { // psp_a sends sequence numbers; bank_b relies on time
				seq++
				e.Sequence = seq
			}
			p.events = append(p.events, e)
		}
		pending := p.paidAt.Add(-g.between(5*time.Second, 2*time.Minute))
		ev(domain.StatusPending, pending, 0)
		if g.rng.Float64() < cfg.ExpireRate {
			p.expired = true
			ev(domain.StatusExpired, pending.Add(15*time.Minute), 0)
			continue
		}
		ev(domain.StatusPaid, p.paidAt, 0)
		if p.provider == ProviderB && g.rng.Float64() < cfg.StaleResendRate {
			ev(domain.StatusPending, p.paidAt.Add(g.between(time.Second, 10*time.Minute)), 0)
		}

		// Refunds happen within hours, before any settlement cut-off, so the
		// provider nets them out of the settled gross.
		at := p.paidAt
		switch r := g.rng.Float64(); {
		case r < cfg.FullRefundRate:
			if g.rng.Intn(3) == 0 {
				at = at.Add(g.between(time.Minute, 3*time.Hour))
				ev(domain.StatusPartiallyRefunded, at, p.amount*int64(10+g.rng.Intn(40))/100/100*100)
			}
			at = at.Add(g.between(time.Minute, 3*time.Hour))
			p.refunded, p.full = p.amount, true
			ev(domain.StatusRefunded, at, p.amount)
		case r < cfg.FullRefundRate+cfg.PartialRefundRate:
			p.refunded = p.amount * int64(10+g.rng.Intn(50)) / 100 / 100 * 100
			at = at.Add(g.between(time.Minute, 3*time.Hour))
			ev(domain.StatusPartiallyRefunded, at, p.refunded)
			if g.rng.Intn(2) == 0 {
				p.refunded += p.amount * int64(5+g.rng.Intn(20)) / 100 / 100 * 100
				at = at.Add(g.between(time.Minute, 3*time.Hour))
				ev(domain.StatusPartiallyRefunded, at, p.refunded)
			}
		}
		rules := g.rules(p.provider)
		gross := p.amount - p.refunded
		fee := rules.Fees[Currency].Fee(gross)
		p.exp = match.Amounts{Gross: gross, Fee: fee, Net: gross - fee}

		posted := p.paidAt.Add(g.between(0, 2*time.Second))
		if g.rng.Intn(100) == 0 { // the occasional slow posting
			posted = p.paidAt.Add(g.between(time.Minute, 30*time.Minute))
		}
		p.ledger = &domain.LedgerTxn{ID: g.uuid(), Provider: p.provider, Reference: p.ref, Amount: p.amount, Currency: Currency,
			CreatedAt: posted, FromAccount: clearing[p.provider], ToAccount: wallet}
	}
}

// nearBoundary returns a time just before a cut-off that matters: UTC midnight
// for psp_a (its T+N date) or a payout-cycle boundary for bank_b. Ledger
// posting delay then pushes the ledger timestamp across the boundary, which
// is exactly the case that must not move the transaction to the wrong window.
func (g *generator) nearBoundary(provider string) time.Time {
	day := g.cfg.Start.AddDate(0, 0, g.rng.Intn(g.cfg.Days))
	var b time.Time
	if provider == ProviderA {
		b = day.Add(24 * time.Hour)
	} else {
		loc := g.rb.Location()
		y, m, d := day.In(loc).Date()
		cycles := 24 / g.cfg.CycleHours
		b = time.Date(y, m, d, 0, 0, 0, 0, loc).Add(time.Duration(1+g.rng.Intn(cycles)) * time.Duration(g.cfg.CycleHours) * time.Hour)
	}
	t := b.Add(-g.between(time.Millisecond, 1500*time.Millisecond)).Truncate(time.Millisecond)
	end := g.cfg.Start.AddDate(0, 0, g.cfg.Days)
	if t.Before(g.cfg.Start) || !t.Before(end) {
		return g.cfg.Start.Add(time.Hour)
	}
	return t
}

func (g *generator) rules(p string) *match.ProviderRules {
	if p == ProviderA {
		return g.ra
	}
	return g.rb
}

func (g *generator) fault(f Fault) {
	g.ds.Truth.Faults = append(g.ds.Truth.Faults, f)
	if f.Class.IsDiscrepancy() {
		g.ds.Truth.Expected[f.Class]++
	}
}

func (g *generator) settledEvent(p *payment, on domain.Date) {
	loc := g.rules(p.provider).Location()
	at := on.StartIn(loc).Add(10*time.Hour + g.between(0, 2*time.Hour))
	if !at.After(p.events[len(p.events)-1].OccurredAt) {
		at = p.events[len(p.events)-1].OccurredAt.Add(time.Second)
	}
	e := domain.PaymentEvent{EventID: g.eventID(), Provider: p.provider, PaymentRef: p.ref, Status: domain.StatusSettled,
		Amount: p.amount, RefundedAmount: p.refunded, Currency: Currency, OccurredAt: at}
	if p.provider == ProviderA {
		e.Sequence = p.events[len(p.events)-1].Sequence + 1
	}
	p.events = append(p.events, e)
	if p.settled == 0 {
		p.settled = on
	}
}

// finish computes webhook delivery order, final statuses, ledger order and
// the as-of time.
func (g *generator) finish() {
	ds := g.ds
	type delivery struct {
		at time.Time
		n  int
		e  domain.PaymentEvent
	}
	var all []delivery
	delay := func() time.Duration {
		switch r := g.rng.Float64(); {
		case r < 0.75:
			return g.between(0, 2*time.Second)
		case r < 0.95:
			return g.between(2*time.Second, 15*time.Minute)
		default:
			return g.between(15*time.Minute, 36*time.Hour)
		}
	}
	var maxDate domain.Date
	for _, p := range g.pays {
		for _, e := range p.events {
			all = append(all, delivery{e.OccurredAt.Add(delay()), len(all), e})
			if g.rng.Float64() < g.cfg.DupEventRate {
				all = append(all, delivery{e.OccurredAt.Add(delay()), len(all), e})
			}
		}
		ds.Truth.FinalStatus[domain.PaymentKey{Provider: p.provider, Ref: p.ref}] = g.intendedStatus(p)
		if p.ledger != nil {
			ds.Ledger = append(ds.Ledger, *p.ledger)
		}
		if p.ledger != nil || p.settled != 0 {
			if dl := domain.DateOf(p.paidAt, g.rules(p.provider).Location()).AddDays(g.rules(p.provider).SettlementDays); dl > maxDate {
				maxDate = dl
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].n < all[j].n
	})
	ds.Events = make([]domain.PaymentEvent, len(all))
	for i, d := range all {
		ds.Events[i] = d.e
	}
	sort.Slice(ds.Ledger, func(i, j int) bool {
		if !ds.Ledger[i].CreatedAt.Equal(ds.Ledger[j].CreatedAt) {
			return ds.Ledger[i].CreatedAt.Before(ds.Ledger[j].CreatedAt)
		}
		return ds.Ledger[i].ID < ds.Ledger[j].ID
	})
	for d := range ds.PSPA {
		maxDate = max(maxDate, d)
	}
	for d := range ds.BankB {
		maxDate = max(maxDate, d)
	}
	ds.Truth.AsOf = maxDate.AddDays(1).StartIn(time.UTC)
}

// intendedStatus is the status the generator meant the payment to end in,
// derived from its own bookkeeping - independently of the state machine, so
// the ground-truth test checks one against the other.
func (g *generator) intendedStatus(p *payment) domain.Status {
	switch {
	case p.expired:
		return domain.StatusExpired
	case p.full:
		return domain.StatusRefunded
	case p.refunded > 0:
		return domain.StatusPartiallyRefunded
	case p.settled != 0:
		return domain.StatusSettled
	}
	return domain.StatusPaid
}
