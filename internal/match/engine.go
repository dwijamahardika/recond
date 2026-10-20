// Package match is the reconciliation engine: a pure function from (ledger
// transactions, settlement lines, payment states, as-of time, rules) to a list
// of outcomes. It performs no I/O and iterates only over explicitly sorted
// slices - never over maps - so the same inputs always produce the same
// outcomes in the same order, which is what makes runs reproducible.
//
// Matching runs in passes, strongest evidence first:
//
//  1. reference  - itemised lines 1:1 on normalised reference; a second line
//     for an already-settled reference is a duplicate settlement.
//  2. refunded   - fully refunded payments owe no settlement.
//  3. tolerance  - remaining itemised lines 1:1 on amount (within tolerance)
//     and provider time (within a window), for missing/mangled references.
//  4. batch      - N:1 payouts against the unmatched transactions in the
//     payout's window, with a bounded subset search (see batch.go).
//  5. leftovers  - unmatched ledger past T+N is missing_in_settlement (or
//     awaiting_settlement before it); unmatched lines are missing_in_ledger.
//
// Each matched group is then checked for amount, fee and timeliness.
package match

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Amounts is an expected or actual gross/fee/net triple in minor units.
type Amounts struct {
	Gross int64 `json:"gross"`
	Fee   int64 `json:"fee"`
	Net   int64 `json:"net"`
}

// Outcome is one verdict over a group of records. Members are indices into the
// Input slices so a million-outcome result does not copy a million IDs.
type Outcome struct {
	Class    domain.Class
	Rule     domain.Rule
	Provider string
	Currency domain.Currency
	Ledger   []int32 // members: ledger transactions
	Lines    []int32 // members: settlement lines
	// Context lists lines cited as evidence without being members (the
	// original settlement a duplicate repeats, the payout an ambiguous
	// exclusion came from).
	Context   []int32
	Expected  Amounts
	Actual    Amounts
	Deadline  domain.Date
	SettledOn domain.Date
	Detail    string
}

// Input is everything a run reconciles.
type Input struct {
	AsOf     time.Time
	Ledger   []domain.LedgerTxn
	Lines    []domain.SettlementLine
	Payments map[domain.PaymentKey]domain.Payment
}

// Result is a run's outcomes plus the input they index into.
type Result struct {
	In       *Input
	Outcomes []Outcome
}

type txnState struct {
	rules    *ProviderRules
	exp      Amounts
	refunded int64
	provTime time.Time // provider clock when known (webhook paid_at), else ledger time
	deadline domain.Date
	matched  bool
	line     int32 // line matched 1:1, or -1
}

type engine struct {
	cfg       *Config
	in        *Input
	txn       []txnState
	used      []bool
	out       []Outcome
	ledOrder  []int32 // ledger by (provider, provider time, id)
	lineOrder []int32 // lines by (settled_on, provider, id)
}

// Reconcile runs every pass and returns the outcomes. It fails only on
// configuration errors (a provider or currency without rules).
func Reconcile(cfg *Config, in *Input) (*Result, error) {
	if cfg.byName == nil {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	if len(in.Ledger) > math.MaxInt32 || len(in.Lines) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: more than %d records; outcomes index records with int32", errConfig, math.MaxInt32)
	}
	e := &engine{cfg: cfg, in: in, txn: make([]txnState, len(in.Ledger)), used: make([]bool, len(in.Lines))}
	if err := e.prepare(); err != nil {
		return nil, err
	}
	e.byReference()
	e.refunded()
	e.byTolerance()
	e.batches()
	e.leftovers()
	return &Result{In: in, Outcomes: e.out}, nil
}

func (e *engine) prepare() error {
	missing := map[string]bool{}
	for i := range e.in.Ledger {
		t := &e.in.Ledger[i]
		r, ok := e.cfg.Provider(t.Provider)
		if !ok {
			missing["provider "+t.Provider] = true
			continue
		}
		fr, ok := r.FeeFor(t.Currency)
		if !ok {
			missing[fmt.Sprintf("fee rule %s/%s", t.Provider, t.Currency)] = true
			continue
		}
		s := txnState{rules: r, provTime: t.CreatedAt, line: -1}
		if p, ok := e.in.Payments[domain.PaymentKey{Provider: t.Provider, Ref: t.Reference}]; ok {
			s.refunded = p.Refunded(t.Amount)
			if !p.PaidAt.IsZero() {
				s.provTime = p.PaidAt
			}
		}
		gross := t.Amount - s.refunded
		fee := fr.Fee(gross)
		s.exp = Amounts{Gross: gross, Fee: fee, Net: gross - fee}
		s.deadline = domain.DateOf(s.provTime, r.Location()).AddDays(r.SettlementDays)
		e.txn[i] = s
	}
	for i := range e.in.Lines {
		if _, ok := e.cfg.Provider(e.in.Lines[i].Provider); !ok {
			missing["provider "+e.in.Lines[i].Provider] = true
		}
	}
	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("%w: no rules for %s", errConfig, strings.Join(keys, ", "))
	}

	e.ledOrder = make([]int32, len(e.in.Ledger))
	for i := range e.ledOrder {
		e.ledOrder[i] = int32(i) //nolint:gosec // bounded by the MaxInt32 check in Reconcile
	}
	sort.Slice(e.ledOrder, func(a, b int) bool {
		x, y := &e.in.Ledger[e.ledOrder[a]], &e.in.Ledger[e.ledOrder[b]]
		if x.Provider != y.Provider {
			return x.Provider < y.Provider
		}
		tx, ty := e.txn[e.ledOrder[a]].provTime, e.txn[e.ledOrder[b]].provTime
		if !tx.Equal(ty) {
			return tx.Before(ty)
		}
		return x.ID < y.ID
	})
	e.lineOrder = make([]int32, len(e.in.Lines))
	for i := range e.lineOrder {
		e.lineOrder[i] = int32(i) //nolint:gosec // bounded by the MaxInt32 check in Reconcile
	}
	sort.Slice(e.lineOrder, func(a, b int) bool {
		x, y := &e.in.Lines[e.lineOrder[a]], &e.in.Lines[e.lineOrder[b]]
		if x.SettledOn != y.SettledOn {
			return x.SettledOn < y.SettledOn
		}
		if x.Provider != y.Provider {
			return x.Provider < y.Provider
		}
		return x.ID < y.ID
	})
	return nil
}

var errConfig = errors.New("reconciliation config")

func (e *engine) emit(o Outcome) { e.out = append(e.out, o) }

// ---- pass 1: reference ----

func (e *engine) byReference() {
	idx := make(map[string][]int32, len(e.in.Ledger))
	for _, i := range e.ledOrder {
		t := &e.in.Ledger[i]
		if ref := domain.NormalizeRef(t.Reference); ref != "" {
			k := t.Provider + "\x00" + ref
			idx[k] = append(idx[k], i)
		}
	}
	for _, li := range e.lineOrder {
		l := &e.in.Lines[li]
		if l.Kind != domain.LineItem {
			continue
		}
		ref := domain.NormalizeRef(l.Reference)
		if ref == "" {
			continue
		}
		cands := idx[l.Provider+"\x00"+ref]
		if len(cands) == 0 {
			continue
		}
		pick := int32(-1)
		for _, c := range cands {
			if !e.txn[c].matched {
				pick = c
				break
			}
		}
		if pick >= 0 {
			e.pair(pick, li, domain.RuleReference, "")
			continue
		}
		// Every ledger transaction with this reference is already settled.
		orig := cands[0]
		for _, c := range cands {
			if ln := e.txn[c].line; ln >= 0 && e.in.Lines[ln].Gross == l.Gross {
				orig = c
				break
			}
		}
		e.duplicate(orig, li)
	}
}

func (e *engine) duplicate(ti, li int32) {
	l := &e.in.Lines[li]
	e.used[li] = true
	o := Outcome{
		Class: domain.ClassDuplicateSettlement, Rule: domain.RuleReference, Provider: l.Provider, Currency: l.Currency,
		Ledger: []int32{ti}, Lines: []int32{li}, Actual: Amounts{l.Gross, l.Fee, l.Net}, SettledOn: l.SettledOn,
		Deadline: e.txn[ti].deadline,
	}
	if ln := e.txn[ti].line; ln >= 0 {
		o.Context = []int32{ln}
		o.Detail = fmt.Sprintf("reference %q already settled by %s on %s", l.Reference, e.in.Lines[ln].ID, e.in.Lines[ln].SettledOn)
	} else {
		o.Detail = fmt.Sprintf("reference %q already settled", l.Reference)
	}
	e.emit(o)
}

// pair matches ledger ti to line li 1:1 and classifies the pair.
func (e *engine) pair(ti, li int32, rule domain.Rule, note string) {
	t, l, s := &e.in.Ledger[ti], &e.in.Lines[li], &e.txn[ti]
	r := s.rules
	s.matched, s.line, e.used[li] = true, li, true

	// The provider's own transaction time, when the line carries it, is the
	// most authoritative basis for T+N.
	deadline := s.deadline
	if !l.TxnAt.IsZero() {
		deadline = domain.DateOf(l.TxnAt, r.Location()).AddDays(r.SettlementDays)
	}
	o := Outcome{
		Class: domain.ClassMatched, Rule: rule, Provider: t.Provider, Currency: t.Currency,
		Ledger: []int32{ti}, Lines: []int32{li}, Expected: s.exp, Actual: Amounts{l.Gross, l.Fee, l.Net},
		Deadline: deadline, SettledOn: l.SettledOn,
	}
	var notes []string
	if note != "" {
		notes = append(notes, note)
	}
	switch {
	case l.Currency != t.Currency:
		o.Class = domain.ClassAmountMismatch
		notes = append(notes, fmt.Sprintf("currency %s in settlement, %s in ledger", l.Currency, t.Currency))
	case !l.FeeKnown && abs(l.Net-s.exp.Net) > r.FeeToleranceMinor+r.AmountToleranceMinor:
		o.Class = domain.ClassAmountMismatch
		notes = append(notes, fmt.Sprintf("net %s, expected %s (fee not reported)", money(l.Net, l.Currency), money(s.exp.Net, l.Currency)))
	case l.FeeKnown && abs(l.Gross-s.exp.Gross) > r.AmountToleranceMinor:
		o.Class = domain.ClassAmountMismatch
		notes = append(notes, fmt.Sprintf("gross %s, expected %s (diff %s)", money(l.Gross, l.Currency), money(s.exp.Gross, l.Currency), signed(l.Gross-s.exp.Gross, l.Currency)))
	case l.FeeKnown && abs(l.Fee-s.exp.Fee) > r.FeeToleranceMinor:
		o.Class = domain.ClassFeeMismatch
		notes = append(notes, fmt.Sprintf("fee %s, expected %s (diff %s)", money(l.Fee, l.Currency), money(s.exp.Fee, l.Currency), signed(l.Fee-s.exp.Fee, l.Currency)))
	}
	if s.refunded > 0 && o.Class != domain.ClassMatched {
		notes = append(notes, fmt.Sprintf("expected gross is net of %s refunded", money(s.refunded, t.Currency)))
	}
	if l.SettledOn > deadline {
		notes = append(notes, fmt.Sprintf("settled %s, due by %s (T+%d)", l.SettledOn, deadline, r.SettlementDays))
		if o.Class == domain.ClassMatched {
			o.Class = domain.ClassLateSettlement
		}
	}
	o.Detail = strings.Join(notes, "; ")
	e.emit(o)
}

// ---- pass 2: refunded ----

func (e *engine) refunded() {
	for _, i := range e.ledOrder {
		s := &e.txn[i]
		if s.matched || s.rules == nil || s.exp.Gross > 0 {
			continue
		}
		s.matched = true
		t := &e.in.Ledger[i]
		e.emit(Outcome{
			Class: domain.ClassMatched, Rule: domain.RuleRefunded, Provider: t.Provider, Currency: t.Currency,
			Ledger: []int32{i}, Deadline: s.deadline,
			Detail: fmt.Sprintf("fully refunded (%s) before settlement; nothing to settle", money(s.refunded, t.Currency)),
		})
	}
}

// ---- pass 3: tolerance ----

type poolKey struct {
	provider string
	currency domain.Currency
}

func (e *engine) unmatchedPools(key func(int32) int64) map[poolKey][]int32 {
	pools := map[poolKey][]int32{}
	for _, i := range e.ledOrder {
		if s := &e.txn[i]; !s.matched && s.rules != nil && s.exp.Gross > 0 {
			k := poolKey{e.in.Ledger[i].Provider, e.in.Ledger[i].Currency}
			pools[k] = append(pools[k], i)
		}
	}
	for _, p := range pools {
		sort.SliceStable(p, func(a, b int) bool { return key(p[a]) < key(p[b]) }) // stable keeps (time, id) order on ties
	}
	return pools
}

func (e *engine) byTolerance() {
	var byGross, byNet map[poolKey][]int32
	gross := func(i int32) int64 { return e.txn[i].exp.Gross }
	net := func(i int32) int64 { return e.txn[i].exp.Net }
	for _, li := range e.lineOrder {
		l := &e.in.Lines[li]
		if e.used[li] || l.Kind != domain.LineItem || l.TxnAt.IsZero() {
			continue
		}
		r, _ := e.cfg.Provider(l.Provider)
		var pool []int32
		val, target, tol := gross, l.Gross, r.AmountToleranceMinor
		if l.FeeKnown {
			if byGross == nil {
				byGross = e.unmatchedPools(gross)
			}
			pool = byGross[poolKey{l.Provider, l.Currency}]
		} else {
			if byNet == nil {
				byNet = e.unmatchedPools(net)
			}
			pool = byNet[poolKey{l.Provider, l.Currency}]
			val, target, tol = net, l.Net, r.AmountToleranceMinor+r.FeeToleranceMinor
		}
		lo := sort.Search(len(pool), func(k int) bool { return val(pool[k]) >= target-tol })
		best, cands := int32(-1), 0
		var bestDT time.Duration
		var bestDA int64
		window := time.Duration(r.MatchWindow)
		for k := lo; k < len(pool) && val(pool[k]) <= target+tol; k++ {
			c := pool[k]
			if e.txn[c].matched {
				continue
			}
			dt := e.txn[c].provTime.Sub(l.TxnAt)
			if dt < 0 {
				dt = -dt
			}
			if dt > window {
				continue
			}
			cands++
			da := abs(val(c) - target)
			if best < 0 || dt < bestDT || (dt == bestDT && da < bestDA) {
				best, bestDT, bestDA = c, dt, da
			}
		}
		if best >= 0 {
			note := fmt.Sprintf("reference %q not found; paired on amount and time (|dt| %s, %d candidate(s), ledger ref %q)",
				l.Reference, bestDT, cands, e.in.Ledger[best].Reference)
			e.pair(best, li, domain.RuleTolerance, note)
		}
	}
}

// ---- pass 5: leftovers ----

func (e *engine) leftovers() {
	for _, i := range e.ledOrder {
		s := &e.txn[i]
		if s.matched || s.rules == nil {
			continue
		}
		t := &e.in.Ledger[i]
		o := Outcome{
			Provider: t.Provider, Currency: t.Currency, Rule: domain.RuleUnmatched, Ledger: []int32{i},
			Expected: s.exp, Deadline: s.deadline,
		}
		if domain.DateOf(e.in.AsOf, s.rules.Location()) > s.deadline {
			o.Class = domain.ClassMissingInSettlement
			o.Detail = fmt.Sprintf("no settlement by %s (T+%d)", s.deadline, s.rules.SettlementDays)
		} else {
			o.Class = domain.ClassAwaitingSettlement
			o.Detail = fmt.Sprintf("not yet due (by %s)", s.deadline)
		}
		e.emit(o)
	}
	for _, li := range e.lineOrder {
		if e.used[li] {
			continue
		}
		l := &e.in.Lines[li]
		e.used[li] = true
		e.emit(Outcome{
			Class: domain.ClassMissingInLedger, Rule: domain.RuleUnmatched, Provider: l.Provider, Currency: l.Currency,
			Lines: []int32{li}, Actual: Amounts{l.Gross, l.Fee, l.Net}, SettledOn: l.SettledOn,
			Detail: fmt.Sprintf("no ledger transaction for reference %q", l.Reference),
		})
	}
}

// ---- helpers ----

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func money(v int64, c domain.Currency) string { return domain.FormatMoney(v, c) }

func signed(v int64, c domain.Currency) string {
	if v > 0 {
		return "+" + money(v, c)
	}
	return money(v, c)
}
