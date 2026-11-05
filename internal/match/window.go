package match

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Windowed runs (docs/adr/0001-windowed-runs-boundary-semantics.md).
//
// A window is an inclusive range of business dates [From, To] plus two
// margins. Every record has one window date, a civil date in its
// provider's timezone:
//
//   - ledger transaction: booking time (created_at)
//   - itemised line: the provider's transaction time, else settled_on - N
//   - payout: the start of the covered period
//
// A windowed run matches every record dated in the visible range
// [From - Lookbehind, To + Lookahead] with the unchanged passes, then
// reports only the groups it owns: those whose earliest record is dated in
// [From, To]. Matching itself (T+N deadlines, payout cycles) still runs on
// the provider's clock: windows decide what a run looks at and which run
// reports a group, never how records pair.
//
// A record whose counterpart is dated outside the visible range is deferred
// as outside_window instead of being called missing:
//
//   - Right after the reference pass, every record still unmatched is looked
//     up by reference outside the visible range. If a partner is there, a
//     full run's reference pass would have taken the record, so it is taken
//     here too - removed from the tolerance and batch pools, which therefore
//     see exactly what a full run's do. With a reference unique on both
//     sides, the partner is attached and the earliest-record rule decides
//     which window reports the pair, so it is reported once.
//   - After the leftovers pass, an owned unmatched transaction whose payout
//     lies outside the visible range, or whose reference partner is
//     ambiguous, is deferred on its own.

// Window margins and length limits.
const (
	DefaultMarginDays = 1
	MaxMarginDays     = 31
	MaxWindowDays     = 366
)

// ErrWindow wraps every window validation failure.
var ErrWindow = errors.New("invalid window")

// Window bounds a run to business dates. Margins are whole calendar days.
type Window struct {
	From       domain.Date `json:"from"`
	To         domain.Date `json:"to"`
	Lookbehind int         `json:"lookbehind_days"`
	Lookahead  int         `json:"lookahead_days"`
}

// WithDefaults returns w with zero margins set to DefaultMarginDays.
func (w Window) WithDefaults() Window {
	if w.Lookbehind == 0 {
		w.Lookbehind = DefaultMarginDays
	}
	if w.Lookahead == 0 {
		w.Lookahead = DefaultMarginDays
	}
	return w
}

// Validate checks dates and margins. Margins below one day cannot absorb
// the gap between the ledger's clock and the provider's around midnight
// (RFC section 6, G3), so they are refused rather than allowed to produce
// false discrepancies.
func (w Window) Validate() error {
	var errs []string
	switch {
	case w.From.IsZero() || w.To.IsZero():
		errs = append(errs, "from and to are required")
	case w.To < w.From:
		errs = append(errs, fmt.Sprintf("to %s is before from %s", w.To, w.From))
	case w.Days() > MaxWindowDays:
		errs = append(errs, fmt.Sprintf("%d days is longer than %d", w.Days(), MaxWindowDays))
	}
	for _, m := range []struct {
		name string
		v    int
	}{{"lookbehind", w.Lookbehind}, {"lookahead", w.Lookahead}} {
		if m.v < 1 || m.v > MaxMarginDays {
			errs = append(errs, fmt.Sprintf("%s must be 1..%d days, got %d", m.name, MaxMarginDays, m.v))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrWindow, strings.Join(errs, "; "))
	}
	return nil
}

// Days is the number of business dates in [From, To].
func (w Window) Days() int { return int(w.To-w.From) + 1 }

// Contains reports whether d is inside [From, To]: groups whose earliest
// record is dated d belong to this window.
func (w Window) Contains(d domain.Date) bool { return d >= w.From && d <= w.To }

// VisibleFrom is the first date the run reads.
func (w Window) VisibleFrom() domain.Date { return w.From.AddDays(-w.Lookbehind) }

// VisibleTo is the last date the run reads.
func (w Window) VisibleTo() domain.Date { return w.To.AddDays(w.Lookahead) }

// Visible reports whether a record dated d is read by the run.
func (w Window) Visible(d domain.Date) bool { return d >= w.VisibleFrom() && d <= w.VisibleTo() }

func (w Window) String() string {
	return fmt.Sprintf("%s..%s (lookbehind %dd, lookahead %dd)", w.From, w.To, w.Lookbehind, w.Lookahead)
}

// LedgerDate is a ledger transaction's window date: its booking time in its
// provider's timezone. (Not the provider's paid_at: that lives in webhook
// state, not on the ledger row, so storage could not select by it.)
func LedgerDate(t *domain.LedgerTxn, r *ProviderRules) domain.Date {
	return domain.DateOf(t.CreatedAt, r.Location())
}

// LineDate is a settlement line's window date in its provider's timezone:
// the provider's transaction time, else the start of the period a payout
// covers, else settled_on minus the settlement term (the latest transaction
// date that could have settled on time). The precedence matches the
// storage index coalesce(txn_at, covers_from).
func LineDate(l *domain.SettlementLine, r *ProviderRules) domain.Date {
	switch {
	case !l.TxnAt.IsZero():
		return domain.DateOf(l.TxnAt, r.Location())
	case !l.CoversFrom.IsZero():
		return domain.DateOf(l.CoversFrom, r.Location())
	}
	return l.SettledOn.AddDays(-r.SettlementDays)
}

// LoadBounds is a superset of a window's visible range that storage can
// select with its indexes. The engine then applies the exact per-provider
// rule, so a loader may read more than this but never less.
type LoadBounds struct {
	// From, To bound ledger created_at and line coalesce(txn_at, covers_from): [From, To).
	From, To time.Time
	// UntimedFrom, UntimedTo bound settled_on (inclusive) of lines with neither time.
	UntimedFrom, UntimedTo domain.Date
}

// Bounds returns the load bounds of w under cfg (validated): the widest
// instants over every configured provider's timezone, and settlement dates
// shifted by the shortest and longest settlement terms.
func (w Window) Bounds(cfg *Config) LoadBounds {
	var b LoadBounds
	minN, maxN := 0, 0
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		from := w.VisibleFrom().StartIn(p.Location())
		to := w.VisibleTo().AddDays(1).StartIn(p.Location())
		if i == 0 || from.Before(b.From) {
			b.From = from
		}
		if i == 0 || to.After(b.To) {
			b.To = to
		}
		if i == 0 || p.SettlementDays < minN {
			minN = p.SettlementDays
		}
		if i == 0 || p.SettlementDays > maxN {
			maxN = p.SettlementDays
		}
	}
	b.UntimedFrom, b.UntimedTo = w.VisibleFrom().AddDays(minN), w.VisibleTo().AddDays(maxN)
	return b
}

// ---- counterpart lookups ----

// Prober finds records that could pair with records a windowed run sees.
// It returns every match regardless of window - the engine decides what is
// outside - and must answer from the same immutable input the run was
// loaded from (storage: the run's file watermark), so a replay asks the same
// questions and gets the same answers.
type Prober interface {
	// LedgerByRef returns provider's ledger transactions whose
	// ProbeKey(reference) is one of keys, by key.
	LedgerByRef(provider string, keys []string) (map[string][]domain.LedgerTxn, error)
	// LinesByRef returns provider's itemised lines whose ProbeKey(reference)
	// is one of keys, by key.
	LinesByRef(provider string, keys []string) (map[string][]domain.SettlementLine, error)
	// PayoutsCovering returns provider's payout lines whose covered period
	// contains at.
	PayoutsCovering(provider string, at time.Time) ([]domain.SettlementLine, error)
}

// ProbeKey normalises a reference for counterpart lookups: surrounding
// spaces trimmed and ASCII letters upper-cased. That is exactly SQLite's
// upper(trim(x)), so an index serves the lookup. Two references with equal
// keys are always equal under domain.NormalizeRef (which the reference pass
// uses); the converse does not hold - NormalizeRef also folds Unicode case
// and other whitespace - so such a partner is not found, and the record
// keeps its ordinary class.
func ProbeKey(ref string) string {
	ref = strings.Trim(ref, " ")
	for i := 0; i < len(ref); i++ {
		if c := ref[i]; c >= 'a' && c <= 'z' {
			b := []byte(ref)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return ref
}

// MemoryProber answers counterpart lookups from records in memory (tests,
// benchmarks, the generator's property tests). Give it every record the
// run could have loaded.
type MemoryProber struct {
	ledger  map[[2]string][]domain.LedgerTxn
	lines   map[[2]string][]domain.SettlementLine
	payouts map[string][]domain.SettlementLine
}

// NewMemoryProber indexes ledger and lines.
func NewMemoryProber(ledger []domain.LedgerTxn, lines []domain.SettlementLine) *MemoryProber {
	m := &MemoryProber{ledger: map[[2]string][]domain.LedgerTxn{}, lines: map[[2]string][]domain.SettlementLine{}, payouts: map[string][]domain.SettlementLine{}}
	for _, t := range ledger {
		k := [2]string{t.Provider, ProbeKey(t.Reference)}
		m.ledger[k] = append(m.ledger[k], t)
	}
	for _, l := range lines {
		switch l.Kind {
		case domain.LineItem:
			k := [2]string{l.Provider, ProbeKey(l.Reference)}
			m.lines[k] = append(m.lines[k], l)
		case domain.LinePayout:
			m.payouts[l.Provider] = append(m.payouts[l.Provider], l)
		}
	}
	return m
}

// LedgerByRef implements Prober.
func (m *MemoryProber) LedgerByRef(provider string, keys []string) (map[string][]domain.LedgerTxn, error) {
	out := map[string][]domain.LedgerTxn{}
	for _, k := range keys {
		if v := m.ledger[[2]string{provider, k}]; len(v) > 0 {
			out[k] = v
		}
	}
	return out, nil
}

// LinesByRef implements Prober.
func (m *MemoryProber) LinesByRef(provider string, keys []string) (map[string][]domain.SettlementLine, error) {
	out := map[string][]domain.SettlementLine{}
	for _, k := range keys {
		if v := m.lines[[2]string{provider, k}]; len(v) > 0 {
			out[k] = v
		}
	}
	return out, nil
}

// PayoutsCovering implements Prober.
func (m *MemoryProber) PayoutsCovering(provider string, at time.Time) ([]domain.SettlementLine, error) {
	var out []domain.SettlementLine
	for _, p := range m.payouts[provider] {
		if !at.Before(p.CoversFrom) && at.Before(p.CoversTo) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ---- engine integration ----

// WindowResult describes how a window shaped a result. Dates are indexed
// like Result.In.Ledger and Result.In.Lines, which hold the visible records
// followed by any counterparts attached from outside the visible range.
type WindowResult struct {
	Window     Window
	LedgerDate []domain.Date
	LineDate   []domain.Date
	// NotOwned counts groups computed over the visible range but owned by a
	// neighbouring window: computed only to give boundary records their
	// counterparts, not reported.
	NotOwned int
	// Deferred counts owned groups classified outside_window.
	Deferred int

	// outside holds, per unmatched visible record, partners found outside
	// the visible range whose reference was not unique ("L:"+index or
	// "S:"+index); they defer the record at the end if it stays unmatched.
	outside map[string][]counterpart
}

// Scope classifies a record by its window date: "window" (owned dates),
// "margin" (visible, not owned) or "outside" (an attached counterpart).
func (w *WindowResult) Scope(d domain.Date) string {
	switch {
	case w.Window.Contains(d):
		return "window"
	case w.Window.Visible(d):
		return "margin"
	}
	return "outside"
}

// restrict returns the visible part of in, with every record's window date.
// Records of a provider without rules are the same configuration error a
// full run reports.
func restrict(cfg *Config, in *Input) (*Input, *WindowResult, error) {
	w := *in.Window
	missing := map[string]bool{}
	ld := make([]domain.Date, len(in.Ledger))
	nl := 0
	for i := range in.Ledger {
		r, ok := cfg.Provider(in.Ledger[i].Provider)
		if !ok {
			missing["provider "+in.Ledger[i].Provider] = true
			continue
		}
		if ld[i] = LedgerDate(&in.Ledger[i], r); w.Visible(ld[i]) {
			nl++
		}
	}
	sd := make([]domain.Date, len(in.Lines))
	ns := 0
	for i := range in.Lines {
		r, ok := cfg.Provider(in.Lines[i].Provider)
		if !ok {
			missing["provider "+in.Lines[i].Provider] = true
			continue
		}
		if sd[i] = LineDate(&in.Lines[i], r); w.Visible(sd[i]) {
			ns++
		}
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("%w: no rules for %s", errConfig, strings.Join(sortedSet(missing), ", "))
	}
	out := &Input{AsOf: in.AsOf, Payments: in.Payments, Window: in.Window, Prober: in.Prober}
	wr := &WindowResult{Window: w, outside: map[string][]counterpart{}}
	if nl == len(in.Ledger) { // storage already selected the visible range: no copy
		out.Ledger, wr.LedgerDate = in.Ledger, ld
	} else {
		out.Ledger, wr.LedgerDate = make([]domain.LedgerTxn, 0, nl), make([]domain.Date, 0, nl)
		for i := range in.Ledger {
			if w.Visible(ld[i]) {
				out.Ledger, wr.LedgerDate = append(out.Ledger, in.Ledger[i]), append(wr.LedgerDate, ld[i])
			}
		}
	}
	if ns == len(in.Lines) {
		out.Lines, wr.LineDate = in.Lines, sd
	} else {
		out.Lines, wr.LineDate = make([]domain.SettlementLine, 0, ns), make([]domain.Date, 0, ns)
		for i := range in.Lines {
			if w.Visible(sd[i]) {
				out.Lines, wr.LineDate = append(out.Lines, in.Lines[i]), append(wr.LineDate, sd[i])
			}
		}
	}
	return out, wr, nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// anchor is the window date of a group's earliest member record.
func (w *WindowResult) anchor(o *Outcome) domain.Date {
	var a domain.Date
	first := true
	for _, i := range o.Ledger {
		if d := w.LedgerDate[i]; first || d < a {
			a, first = d, false
		}
	}
	for _, i := range o.Lines {
		if d := w.LineDate[i]; first || d < a {
			a, first = d, false
		}
	}
	return a
}

// counterpart is a record found outside the visible range.
type counterpart struct {
	kind   string // "ledger transaction", "settlement line", "payout"
	id     string
	date   domain.Date
	ledger *domain.LedgerTxn
	line   *domain.SettlementLine
}

// deferByReference runs after the reference pass. Every record still
// unmatched is looked up by reference outside the visible range; one with a
// partner there would have been taken by a full run's reference pass, so it
// is taken here (kept out of the later passes) and deferred. A partner
// unique by reference on both sides is attached.
func (e *engine) deferByReference(w *WindowResult) error {
	p := e.in.Prober
	type rec struct {
		ledger int32 // -1 for a line
		line   int32
	}
	// Unmatched visible records by (provider, key), in pass order.
	ledKeys, lineKeys := map[string][]string{}, map[string][]string{}
	ledRecs, lineRecs := map[[2]string][]int32{}, map[[2]string][]int32{}
	for _, i := range e.ledOrder {
		if s := &e.txn[i]; s.matched || s.rules == nil {
			continue
		}
		t := &e.in.Ledger[i]
		if k := ProbeKey(t.Reference); k != "" {
			pk := [2]string{t.Provider, k}
			if len(ledRecs[pk]) == 0 {
				ledKeys[t.Provider] = append(ledKeys[t.Provider], k)
			}
			ledRecs[pk] = append(ledRecs[pk], i)
		}
	}
	for _, li := range e.lineOrder {
		l := &e.in.Lines[li]
		if e.used[li] || l.Kind != domain.LineItem {
			continue
		}
		if k := ProbeKey(l.Reference); k != "" {
			pk := [2]string{l.Provider, k}
			if len(lineRecs[pk]) == 0 {
				lineKeys[l.Provider] = append(lineKeys[l.Provider], k)
			}
			lineRecs[pk] = append(lineRecs[pk], li)
		}
	}
	type decision struct {
		found  []counterpart
		unique bool
	}
	decided := map[rec]decision{}

	for _, prov := range sortedKeys(ledKeys) {
		r, _ := e.cfg.Provider(prov)
		lines, err := p.LinesByRef(prov, ledKeys[prov])
		if err != nil {
			return err
		}
		var check []string // keys whose single partner is outside: is the ledger side unique too?
		for _, k := range ledKeys[prov] {
			all := lines[k]
			var found []counterpart
			for j := range all {
				if d := LineDate(&all[j], r); !w.Window.Visible(d) {
					found = append(found, counterpart{kind: "settlement line", id: all[j].ID, date: d, line: &all[j]})
				}
			}
			if len(found) == 0 {
				continue
			}
			if len(all) == 1 && len(ledRecs[[2]string{prov, k}]) == 1 {
				check = append(check, k)
			}
			for _, i := range ledRecs[[2]string{prov, k}] {
				decided[rec{ledger: i, line: -1}] = decision{found: found}
			}
		}
		if len(check) > 0 {
			back, err := p.LedgerByRef(prov, check)
			if err != nil {
				return err
			}
			for _, k := range check {
				i := ledRecs[[2]string{prov, k}][0]
				if b := back[k]; len(b) == 1 && b[0].ID == e.in.Ledger[i].ID {
					d := decided[rec{ledger: i, line: -1}]
					d.unique = true
					decided[rec{ledger: i, line: -1}] = d
				}
			}
		}
	}
	for _, prov := range sortedKeys(lineKeys) {
		r, _ := e.cfg.Provider(prov)
		txns, err := p.LedgerByRef(prov, lineKeys[prov])
		if err != nil {
			return err
		}
		var check []string
		for _, k := range lineKeys[prov] {
			all := txns[k]
			var found []counterpart
			for j := range all {
				if d := LedgerDate(&all[j], r); !w.Window.Visible(d) {
					found = append(found, counterpart{kind: "ledger transaction", id: all[j].ID, date: d, ledger: &all[j]})
				}
			}
			if len(found) == 0 {
				continue
			}
			if len(all) == 1 && len(lineRecs[[2]string{prov, k}]) == 1 {
				check = append(check, k)
			}
			for _, li := range lineRecs[[2]string{prov, k}] {
				decided[rec{ledger: -1, line: li}] = decision{found: found}
			}
		}
		if len(check) > 0 {
			back, err := p.LinesByRef(prov, check)
			if err != nil {
				return err
			}
			for _, k := range check {
				li := lineRecs[[2]string{prov, k}][0]
				if b := back[k]; len(b) == 1 && b[0].ID == e.in.Lines[li].ID {
					d := decided[rec{ledger: -1, line: li}]
					d.unique = true
					decided[rec{ledger: -1, line: li}] = d
				}
			}
		}
	}

	// Unique partners: take the record, attach the partner, defer the pair.
	// Ambiguous ones are remembered for the leftovers. Pass order keeps the
	// emitted outcomes deterministic.
	for _, i := range e.ledOrder {
		d, ok := decided[rec{ledger: i, line: -1}]
		if !ok {
			continue
		}
		if !d.unique {
			w.outside["L:"+itoa32(i)] = d.found
			continue
		}
		s, t, c := &e.txn[i], &e.in.Ledger[i], d.found[0]
		s.matched = true
		o := Outcome{Class: domain.ClassOutsideWindow, Rule: domain.RuleReference, Provider: t.Provider, Currency: t.Currency,
			Ledger: []int32{i}, Expected: s.exp, Deadline: s.deadline}
		e.attach(w, &o, c)
		o.Detail = deferDetail(w, c, nil, "attached", "pairs by reference")
		e.emit(o)
	}
	for _, li := range e.lineOrder {
		d, ok := decided[rec{ledger: -1, line: li}]
		if !ok {
			continue
		}
		if !d.unique {
			w.outside["S:"+itoa32(li)] = d.found
			continue
		}
		l, c := &e.in.Lines[li], d.found[0]
		e.used[li] = true
		o := Outcome{Class: domain.ClassOutsideWindow, Rule: domain.RuleReference, Provider: l.Provider, Currency: l.Currency,
			Lines: []int32{li}, Actual: Amounts{l.Gross, l.Fee, l.Net}, SettledOn: l.SettledOn}
		e.attach(w, &o, c)
		o.Detail = deferDetail(w, c, nil, "attached", "pairs by reference")
		e.emit(o)
	}
	return nil
}

// own keeps the groups the window owns, and defers owned leftovers whose
// payout or (ambiguous) reference partner lies outside the visible range.
func (e *engine) own(w *WindowResult, res *Result) error {
	kept := res.Outcomes[:0]
	for k := range res.Outcomes {
		o := res.Outcomes[k]
		if !w.Window.Contains(w.anchor(&o)) {
			w.NotOwned++
			continue
		}
		if o.Rule == domain.RuleUnmatched {
			if err := e.deferLeftover(w, &o); err != nil {
				return fmt.Errorf("counterpart lookup: %w", err)
			}
		}
		if o.Class == domain.ClassOutsideWindow {
			w.Deferred++
		}
		kept = append(kept, o)
	}
	res.Outcomes = kept
	return nil
}

// deferLeftover defers an owned unmatched record whose counterpart lies
// outside the visible range: an ambiguous reference partner found after the
// reference pass, or (for a transaction) a payout covering its provider
// time. It is deferred on its own; with no such counterpart it keeps its
// class (a real discrepancy, or awaiting).
func (e *engine) deferLeftover(w *WindowResult, o *Outcome) error {
	var found []counterpart
	switch {
	case len(o.Ledger) == 1 && len(o.Lines) == 0:
		i := o.Ledger[0]
		found = append(found, w.outside["L:"+itoa32(i)]...)
		t, s := &e.in.Ledger[i], &e.txn[i]
		payouts, err := e.in.Prober.PayoutsCovering(t.Provider, s.provTime)
		if err != nil {
			return err
		}
		for j := range payouts {
			if d := LineDate(&payouts[j], s.rules); !w.Window.Visible(d) {
				found = append(found, counterpart{kind: "payout", id: payouts[j].ID, date: d, line: &payouts[j]})
			}
		}
	case len(o.Lines) == 1 && len(o.Ledger) == 0:
		found = w.outside["S:"+itoa32(o.Lines[0])]
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(a, b int) bool {
		if found[a].date != found[b].date {
			return found[a].date < found[b].date
		}
		return found[a].id < found[b].id
	})
	orig := o.Class
	o.Class = domain.ClassOutsideWindow
	o.Detail = deferDetail(w, found[0], found[1:], "not attached: reference not unique, or a payout", "would be "+string(orig))
	return nil
}

func deferDetail(w *WindowResult, c counterpart, more []counterpart, attached, what string) string {
	widen := "lookahead"
	if c.date < w.Window.VisibleFrom() {
		widen = "lookbehind"
	}
	extra := ""
	if len(more) > 0 {
		extra = fmt.Sprintf(" (+%d more)", len(more))
	}
	return fmt.Sprintf("counterpart outside window: %s %s dated %s%s, visible range %s..%s (%s); verdict deferred (%s); widen -%s to decide here",
		c.kind, c.id, c.date, extra, w.Window.VisibleFrom(), w.Window.VisibleTo(), attached, what, widen)
}

// attach appends a counterpart from outside the visible range to the
// engine's input and makes it a member of o. The input slices are re-sliced
// to their length first, so an append can never write into a caller's
// array. Attached records are not in the passes' sort orders, so no pass
// sees them.
func (e *engine) attach(w *WindowResult, o *Outcome, c counterpart) {
	in := e.in
	switch {
	case c.ledger != nil:
		in.Ledger = append(in.Ledger[:len(in.Ledger):len(in.Ledger)], *c.ledger)
		w.LedgerDate = append(w.LedgerDate, c.date)
		o.Ledger = append(o.Ledger, int32(len(in.Ledger)-1)) //nolint:gosec // bounded like the input
	case c.line != nil:
		in.Lines = append(in.Lines[:len(in.Lines):len(in.Lines)], *c.line)
		w.LineDate = append(w.LineDate, c.date)
		o.Lines = append(o.Lines, int32(len(in.Lines)-1)) //nolint:gosec // bounded like the input
		o.Actual = Amounts{c.line.Gross, c.line.Fee, c.line.Net}
		o.SettledOn = c.line.SettledOn
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itoa32(i int32) string { return fmt.Sprint(i) }

// AnchorDate is the window date of outcome i's earliest record: the window
// that owns it. Every run computes it, full or windowed, so the exception
// queue can scope a later windowed run. Zero if the result was not built by
// Reconcile.
func (r *Result) AnchorDate(i int) domain.Date {
	o := &r.Outcomes[i]
	if r.Window != nil {
		return r.Window.anchor(o)
	}
	if r.cfg == nil {
		return 0
	}
	var a domain.Date
	first := true
	for _, j := range o.Ledger {
		t := &r.In.Ledger[j]
		if p, ok := r.cfg.Provider(t.Provider); ok {
			if d := LedgerDate(t, p); first || d < a {
				a, first = d, false
			}
		}
	}
	for _, j := range o.Lines {
		l := &r.In.Lines[j]
		if p, ok := r.cfg.Provider(l.Provider); ok {
			if d := LineDate(l, p); first || d < a {
				a, first = d, false
			}
		}
	}
	return a
}
