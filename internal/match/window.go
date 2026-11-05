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
// [From, To]. A record it owns but left unmatched, whose counterpart exists
// outside the visible range, is deferred as outside_window rather than
// called missing; when the reference is unique on both sides the
// counterpart is attached and the earliest-record rule decides which
// window reports the pair. Matching itself (T+N deadlines, payout cycles)
// still runs on the provider's clock: windows decide what a run looks at
// and which run reports a group, never how records pair.

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

// Prober finds records outside a window's visible range that could pair
// with a record the window left unmatched. It must answer from the same
// immutable input the run was loaded from (storage: the run's file
// watermark), so a replay asks the same questions and gets the same answers.
type Prober interface {
	// LedgerByRef returns provider's ledger transactions whose ProbeKey(reference) is key.
	LedgerByRef(provider, key string) ([]domain.LedgerTxn, error)
	// LinesByRef returns provider's itemised lines whose ProbeKey(reference) is key.
	LinesByRef(provider, key string) ([]domain.SettlementLine, error)
	// PayoutsCovering returns provider's payout lines whose covered period contains at.
	PayoutsCovering(provider string, at time.Time) ([]domain.SettlementLine, error)
}

// ProbeKey normalises a reference for counterpart lookups: surrounding
// spaces trimmed and ASCII letters upper-cased. That is exactly SQLite's
// upper(trim(x)), so an index serves the lookup. It is narrower than
// domain.NormalizeRef, which also folds Unicode case and other whitespace;
// a counterpart that differs only that way is not found, and the record
// keeps its ordinary leftover class.
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
func (m *MemoryProber) LedgerByRef(provider, key string) ([]domain.LedgerTxn, error) {
	return m.ledger[[2]string{provider, key}], nil
}

// LinesByRef implements Prober.
func (m *MemoryProber) LinesByRef(provider, key string) ([]domain.SettlementLine, error) {
	return m.lines[[2]string{provider, key}], nil
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
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, nil, fmt.Errorf("%w: no rules for %s", errConfig, strings.Join(keys, ", "))
	}
	out := &Input{AsOf: in.AsOf, Payments: in.Payments, Window: in.Window, Prober: in.Prober}
	wr := &WindowResult{Window: w}
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

// own keeps the groups the window owns and defers owned leftovers whose
// counterpart lies outside the visible range.
func (e *engine) own(w *WindowResult, res *Result) error {
	kept := res.Outcomes[:0]
	for k := range res.Outcomes {
		o := res.Outcomes[k]
		if !w.Window.Contains(w.anchor(&o)) {
			w.NotOwned++
			continue
		}
		if o.Rule == domain.RuleUnmatched {
			keep, err := e.deferOutside(w, res, &o)
			if err != nil {
				return fmt.Errorf("counterpart lookup: %w", err)
			}
			if !keep {
				w.NotOwned++
				continue
			}
		}
		kept = append(kept, o)
	}
	res.Outcomes = kept
	return nil
}

// counterpart is a record found outside the visible range.
type counterpart struct {
	kind   string // "ledger transaction", "settlement line", "payout"
	id     string
	date   domain.Date
	ledger *domain.LedgerTxn
	line   *domain.SettlementLine
}

// deferOutside looks for the counterpart of an owned leftover outside the
// visible range. With none, the outcome is unchanged (a real discrepancy, or
// awaiting). With one, it becomes outside_window; a counterpart unique by
// reference on both sides is attached, and if it is earlier than the window
// the earlier window reports the pair (keep = false).
func (e *engine) deferOutside(w *WindowResult, res *Result, o *Outcome) (keep bool, err error) {
	p := res.In.Prober
	var found []counterpart
	unique := false
	switch {
	case len(o.Ledger) == 1 && len(o.Lines) == 0:
		i := o.Ledger[0]
		t := &res.In.Ledger[i]
		r := e.txn[i].rules
		var lines []domain.SettlementLine
		if key := ProbeKey(t.Reference); key != "" {
			if lines, err = p.LinesByRef(t.Provider, key); err != nil {
				return false, err
			}
		}
		payouts, err := p.PayoutsCovering(t.Provider, e.txn[i].provTime)
		if err != nil {
			return false, err
		}
		for j := range lines {
			if d := LineDate(&lines[j], r); !w.Window.Visible(d) {
				found = append(found, counterpart{kind: "settlement line", id: lines[j].ID, date: d, line: &lines[j]})
			}
		}
		byRef := len(found)
		for j := range payouts {
			if d := LineDate(&payouts[j], r); !w.Window.Visible(d) {
				found = append(found, counterpart{kind: "payout", id: payouts[j].ID, date: d, line: &payouts[j]})
			}
		}
		if byRef == 1 && len(found) == 1 {
			back, err := p.LedgerByRef(t.Provider, ProbeKey(found[0].line.Reference))
			if err != nil {
				return false, err
			}
			unique = len(back) == 1 && back[0].ID == t.ID
		}
	case len(o.Lines) == 1 && len(o.Ledger) == 0 && res.In.Lines[o.Lines[0]].Kind == domain.LineItem:
		l := &res.In.Lines[o.Lines[0]]
		key := ProbeKey(l.Reference)
		if key == "" {
			return true, nil
		}
		r, _ := e.cfg.Provider(l.Provider)
		txns, err := p.LedgerByRef(l.Provider, key)
		if err != nil {
			return false, err
		}
		for j := range txns {
			if d := LedgerDate(&txns[j], r); !w.Window.Visible(d) {
				found = append(found, counterpart{kind: "ledger transaction", id: txns[j].ID, date: d, ledger: &txns[j]})
			}
		}
		if len(found) == 1 {
			back, err := p.LinesByRef(l.Provider, ProbeKey(found[0].ledger.Reference))
			if err != nil {
				return false, err
			}
			unique = len(back) == 1 && back[0].ID == l.ID
		}
	default:
		return true, nil
	}
	if len(found) == 0 {
		return true, nil
	}
	sort.Slice(found, func(a, b int) bool {
		if found[a].date != found[b].date {
			return found[a].date < found[b].date
		}
		return found[a].id < found[b].id
	})
	orig := o.Class
	o.Class = domain.ClassOutsideWindow
	c := found[0]
	widen := "lookahead"
	if c.date < w.Window.VisibleFrom() {
		widen = "lookbehind"
	}
	more := ""
	if len(found) > 1 {
		more = fmt.Sprintf(" (+%d more)", len(found)-1)
	}
	attached := "not attached: reference not unique or a payout"
	if unique {
		attached = "attached"
		e.attach(w, res, o, c)
		if w.anchor(o) < w.Window.From {
			return false, nil // the earlier window holding the counterpart reports this pair
		}
	}
	o.Detail = fmt.Sprintf("counterpart outside window: %s %s dated %s%s, visible range %s..%s (%s); verdict deferred, would be %s; widen -%s to decide here",
		c.kind, c.id, c.date, more, w.Window.VisibleFrom(), w.Window.VisibleTo(), attached, orig, widen)
	w.Deferred++
	return true, nil
}

// attach appends a counterpart from outside the visible range to the
// result's input and makes it a member of o. The input slices are re-sliced
// to their length first so an append can never write into a caller's array.
func (e *engine) attach(w *WindowResult, res *Result, o *Outcome, c counterpart) {
	in := res.In
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
