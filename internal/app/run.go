package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

// ClassStat aggregates one outcome class.
type ClassStat struct {
	Class      domain.Class `json:"class"`
	Outcomes   int          `json:"outcomes"`
	LedgerTxns int          `json:"ledger_txns"`
	Lines      int          `json:"settlement_lines"`
	// AbsNetDelta sums |actual net - expected net| per currency, in minor units.
	AbsNetDelta map[domain.Currency]int64 `json:"abs_net_delta_minor,omitempty"`
}

// ProviderStat aggregates one provider.
type ProviderStat struct {
	Provider   string  `json:"provider"`
	LedgerTxns int     `json:"ledger_txns"`
	Lines      int     `json:"settlement_lines"`
	Matched    int     `json:"ledger_txns_matched"`
	Flagged    int     `json:"ledger_txns_flagged"`
	MatchRate  float64 `json:"match_rate"` // a ratio for humans, not money
}

// Summary is the JSON summary of a run.
type Summary struct {
	RunID         int64                `json:"run_id"`
	AsOf          time.Time            `json:"as_of"`
	EngineVersion string               `json:"engine_version"`
	ConfigSHA256  string               `json:"config_sha256"`
	InputDigest   string               `json:"input_digest"`
	ResultDigest  string               `json:"result_digest"`
	MaxFileID     int64                `json:"max_file_id"`
	MaxEventSeq   int64                `json:"max_event_seq"`
	LedgerTxns    int                  `json:"ledger_txns"`
	Lines         int                  `json:"settlement_lines"`
	Payments      int                  `json:"payments"`
	Outcomes      int                  `json:"outcomes"`
	Discrepancies int                  `json:"discrepancies"`
	Classes       []ClassStat          `json:"classes"`
	Providers     []ProviderStat       `json:"providers"`
	Exceptions    sqlite.ExceptionSync `json:"exceptions"`
	Timings       map[string]int64     `json:"timings_ms"`
	// Window is present only for windowed runs; a full run's summary is
	// exactly what it was before windowed runs existed.
	Window *WindowSummary `json:"window,omitempty"`
}

// WindowSummary says what a windowed run covered and what it saw around it.
// Ledger and line counts elsewhere in the summary count the records the run
// reports (members of the groups it owns); the counts here are by each
// record's own window date.
type WindowSummary struct {
	match.Window
	Days int `json:"days"`
	// Providers gives each provider's window as instants: business dates
	// are civil dates in each provider's own timezone.
	Providers      []WindowBounds `json:"providers"`
	LedgerInWindow int            `json:"ledger_txns_in_window"`
	LedgerInMargin int            `json:"ledger_txns_in_margin"`
	LinesInWindow  int            `json:"settlement_lines_in_window"`
	LinesInMargin  int            `json:"settlement_lines_in_margin"`
	// Attached counts counterparts from outside the visible range attached
	// to outside_window groups.
	Attached int `json:"counterparts_attached"`
	// GroupsNotOwned were computed over the visible range only to give
	// boundary records their counterparts; a neighbouring window owns them.
	GroupsNotOwned int `json:"groups_not_owned"`
	// OutsideWindow counts deferred groups: should be 0 with adequate margins.
	OutsideWindow int `json:"outside_window"`
}

// WindowBounds is one provider's window [From, To) as instants.
type WindowBounds struct {
	Provider string    `json:"provider"`
	Timezone string    `json:"timezone"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

func windowSummary(rules *match.Config, wr *match.WindowResult) *WindowSummary {
	w := wr.Window
	ws := &WindowSummary{Window: w, Days: w.Days(), GroupsNotOwned: wr.NotOwned, OutsideWindow: wr.Deferred}
	for _, name := range rules.Names() {
		p, _ := rules.Provider(name)
		ws.Providers = append(ws.Providers, WindowBounds{Provider: name, Timezone: p.Timezone,
			From: w.From.StartIn(p.Location()).UTC(), To: w.To.AddDays(1).StartIn(p.Location()).UTC()})
	}
	count := func(dates []domain.Date, in, margin *int) {
		for _, d := range dates {
			switch wr.Scope(d) {
			case "window":
				*in++
			case "margin":
				*margin++
			default:
				ws.Attached++
			}
		}
	}
	count(wr.LedgerDate, &ws.LedgerInWindow, &ws.LedgerInMargin)
	count(wr.LineDate, &ws.LinesInWindow, &ws.LinesInMargin)
	return ws
}

func summarize(res *match.Result) Summary {
	// A full run reports every record it read. A windowed run reports the
	// records of the groups it owns: those partition across consecutive
	// windows exactly as groups do, and keep match rates within 100%
	// (dating by record would not: a payout member booked after midnight
	// is dated in the margin but belongs to a group this window owns).
	classes := domain.Classes
	inLedger := func(int) bool { return true }
	inLines := func(int) bool { return true }
	if res.Window != nil {
		classes = domain.WindowClasses
		led, lin := map[int]bool{}, map[int]bool{}
		for i := range res.Outcomes {
			for _, j := range res.Outcomes[i].Ledger {
				led[int(j)] = true
			}
			for _, j := range res.Outcomes[i].Lines {
				lin[int(j)] = true
			}
		}
		inLedger = func(i int) bool { return led[i] }
		inLines = func(i int) bool { return lin[i] }
	}
	byClass := map[domain.Class]*ClassStat{}
	for _, c := range classes {
		byClass[c] = &ClassStat{Class: c}
	}
	prov := map[string]*ProviderStat{}
	p := func(name string) *ProviderStat {
		if prov[name] == nil {
			prov[name] = &ProviderStat{Provider: name}
		}
		return prov[name]
	}
	sum := Summary{Payments: len(res.In.Payments), Outcomes: len(res.Outcomes)}
	for i, t := range res.In.Ledger {
		if inLedger(i) {
			p(t.Provider).LedgerTxns++
			sum.LedgerTxns++
		}
	}
	for i, l := range res.In.Lines {
		if inLines(i) {
			p(l.Provider).Lines++
			sum.Lines++
		}
	}
	for i := range res.Outcomes {
		o := &res.Outcomes[i]
		cs := byClass[o.Class]
		cs.Outcomes++
		cs.LedgerTxns += len(o.Ledger)
		cs.Lines += len(o.Lines)
		if o.Class.IsDiscrepancy() {
			sum.Discrepancies++
			if cs.AbsNetDelta == nil {
				cs.AbsNetDelta = map[domain.Currency]int64{}
			}
			d := o.Actual.Net - o.Expected.Net
			if d < 0 {
				d = -d
			}
			cs.AbsNetDelta[o.Currency] += d
		}
		switch {
		case o.Class == domain.ClassMatched:
			p(o.Provider).Matched += len(o.Ledger)
		case o.Class.IsDiscrepancy() && o.Class != domain.ClassDuplicateSettlement:
			p(o.Provider).Flagged += len(o.Ledger)
		}
	}
	for _, c := range classes {
		sum.Classes = append(sum.Classes, *byClass[c])
	}
	names := make([]string, 0, len(prov))
	for n := range prov {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps := prov[n]
		if ps.LedgerTxns > 0 {
			ps.MatchRate = float64(ps.Matched) / float64(ps.LedgerTxns)
		}
		sum.Providers = append(sum.Providers, *ps)
	}
	return sum
}

// RunOptions configures a run.
type RunOptions struct {
	AsOf time.Time // zero: now
	// Window restricts the run to business dates (zero margins mean the
	// default); nil reconciles the whole history. Needs the feature flag.
	Window *match.Window
}

// Run reconciles everything ingested so far - or, with a window, the
// window's business dates - and stores the result.
func (s *Service) Run(ctx context.Context, opt RunOptions) (*Summary, error) {
	t0 := s.clock.Now()
	if opt.AsOf.IsZero() {
		opt.AsOf = t0
	}
	if opt.Window != nil {
		if !s.windowed {
			return nil, ErrWindowedRunsDisabled
		}
		w := opt.Window.WithDefaults()
		if err := w.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		opt.Window = &w
	}
	timings := map[string]int64{}
	mark := func(name string, since time.Time) time.Time {
		now := time.Now()
		timings[name] = now.Sub(since).Milliseconds()
		return now
	}
	start := time.Now()
	var in *sqlite.Inputs
	var err error
	if opt.Window != nil {
		in, err = s.store.LoadWindow(ctx, opt.Window.Bounds(s.rules))
	} else {
		in, err = s.store.LoadInputs(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("load inputs: %w", err)
	}
	step := mark("load", start)
	mi := &match.Input{AsOf: opt.AsOf.UTC(), Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments}
	if opt.Window != nil {
		mi.Window, mi.Prober = opt.Window, s.store.Prober(ctx, in.MaxFileID)
	}
	res, err := match.Reconcile(s.rules, mi)
	if err != nil {
		return nil, err
	}
	step = mark("match", step)
	digest := res.Digest()
	cfgJSON, err := s.rules.Canonical()
	if err != nil {
		return nil, err
	}
	cfgSum := sha256.Sum256(cfgJSON)
	evidence, err := s.evidence(ctx, res)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	step = mark("evidence", step)

	sum := summarize(res)
	sum.AsOf, sum.EngineVersion, sum.ConfigSHA256 = opt.AsOf.UTC(), match.EngineVersion, hex.EncodeToString(cfgSum[:])
	sum.InputDigest, sum.ResultDigest, sum.MaxFileID, sum.MaxEventSeq = in.Digest, digest, in.MaxFileID, in.MaxEventSeq
	if res.Window != nil {
		sum.Window = windowSummary(s.rules, res.Window)
	}
	summaryJSON, _ := json.Marshal(sum)
	run := sqlite.Run{
		AsOf: opt.AsOf.UTC(), EngineVersion: match.EngineVersion, ConfigJSON: string(cfgJSON), ConfigSHA256: sum.ConfigSHA256,
		MaxFileID: in.MaxFileID, MaxEventSeq: in.MaxEventSeq, InputDigest: in.Digest, ResultDigest: digest,
		LedgerCount: sum.LedgerTxns, LineCount: sum.Lines, PaymentCount: len(in.Payments),
		SummaryJSON: string(summaryJSON), DurationMS: time.Since(start).Milliseconds(), Window: opt.Window,
	}
	runID, sync, err := s.store.SaveRun(ctx, run, res, evidence)
	if err != nil {
		return nil, fmt.Errorf("save run: %w", err)
	}
	mark("save", step)
	timings["total"] = time.Since(start).Milliseconds()
	sum.RunID, sum.Exceptions, sum.Timings = runID, sync, timings

	counts := res.Counts()
	s.metrics.RunFinished(time.Since(start), counts)
	if c, err := s.store.ExceptionCounts(ctx); err == nil {
		s.metrics.ExceptionsOpen(c[sqlite.StatusOpen])
	}
	attrs := []any{"run_id", runID, "ledger", sum.LedgerTxns, "lines", sum.Lines, "discrepancies", sum.Discrepancies,
		"opened", sync.Opened, "auto_closed", sync.AutoClosed, "duration_ms", timings["total"], "digest", digest}
	if w := sum.Window; w != nil {
		attrs = append(attrs, "window", w.String(), "ledger_in_margin", w.LedgerInMargin, "lines_in_margin", w.LinesInMargin,
			"groups_not_owned", w.GroupsNotOwned, "outside_window", w.OutsideWindow)
		if w.OutsideWindow > 0 {
			s.log.Warn("windowed run deferred groups whose counterpart is outside its margins", "run_id", runID,
				"window", w.String(), "outside_window", w.OutsideWindow)
		}
	}
	s.log.Info("run complete", attrs...)
	return &sum, nil
}

// Summary returns a stored run's summary.
func (s *Service) Summary(ctx context.Context, runID int64) (*Summary, sqlite.Run, error) {
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return nil, run, err
	}
	var sum Summary
	if err := json.Unmarshal([]byte(run.SummaryJSON), &sum); err != nil {
		return nil, run, err
	}
	sum.RunID = run.ID
	return &sum, run, nil
}

// Runs lists recent runs.
func (s *Service) Runs(ctx context.Context, limit int) ([]sqlite.Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	return s.store.Runs(ctx, limit)
}

// Verification is the result of replaying a stored run.
type Verification struct {
	RunID          int64  `json:"run_id"`
	EngineVersion  string `json:"engine_version"`
	CurrentEngine  string `json:"current_engine"`
	InputDigest    string `json:"input_digest"`
	InputMatches   bool   `json:"input_matches"`
	ResultDigest   string `json:"result_digest"`
	ReplayDigest   string `json:"replay_digest"`
	Reproduced     bool   `json:"reproduced"`
	ReplayDuration int64  `json:"replay_ms"`
	// Window is the stored window the replay used (nil: full history).
	Window *match.Window `json:"window,omitempty"`
}

// Verify re-runs a stored run from its recorded watermarks, as-of time and
// rules (not the current ones), folding payment state from the event log, and
// compares digests.
func (s *Service) Verify(ctx context.Context, runID int64) (*Verification, error) {
	start := time.Now()
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	cfg, err := match.ParseConfig([]byte(run.ConfigJSON))
	if err != nil {
		return nil, fmt.Errorf("stored config: %w", err)
	}
	// A windowed run replays with its stored window and the stored rules'
	// timezones, and asks the same counterpart questions at its watermark.
	// Verify never depends on the feature flag.
	var in *sqlite.Inputs
	if run.Window != nil {
		in, err = s.store.LoadWindowAt(ctx, run.MaxFileID, run.MaxEventSeq, run.Window.Bounds(cfg))
	} else {
		in, err = s.store.LoadInputsAt(ctx, run.MaxFileID, run.MaxEventSeq)
	}
	if err != nil {
		return nil, err
	}
	mi := &match.Input{AsOf: run.AsOf, Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments}
	if run.Window != nil {
		mi.Window, mi.Prober = run.Window, s.store.Prober(ctx, run.MaxFileID)
	}
	res, err := match.Reconcile(cfg, mi)
	if err != nil {
		return nil, err
	}
	v := &Verification{
		RunID: run.ID, EngineVersion: run.EngineVersion, CurrentEngine: match.EngineVersion,
		InputDigest: run.InputDigest, InputMatches: in.Digest == run.InputDigest,
		ResultDigest: run.ResultDigest, ReplayDigest: res.Digest(), Window: run.Window,
	}
	v.Reproduced = v.InputMatches && v.ReplayDigest == v.ResultDigest
	v.ReplayDuration = time.Since(start).Milliseconds()
	return v, nil
}

// ---- evidence ----

// Evidence is what a reviewer needs to decide an exception without opening
// another system: both sides' records (settlement rows verbatim), the
// provider's payment state, and what recond expected.
type Evidence struct {
	Class     domain.Class            `json:"class"`
	Rule      domain.Rule             `json:"rule"`
	Detail    string                  `json:"detail"`
	Expected  match.Amounts           `json:"expected"`
	Actual    match.Amounts           `json:"actual"`
	Deadline  domain.Date             `json:"deadline,omitempty"`
	SettledOn domain.Date             `json:"settled_on,omitempty"`
	Ledger    []domain.LedgerTxn      `json:"ledger"`
	Lines     []domain.SettlementLine `json:"settlement"`
	Context   []domain.SettlementLine `json:"context,omitempty"`
	Payments  []domain.Payment        `json:"payments,omitempty"`
	Truncated int                     `json:"truncated_records,omitempty"`
}

const maxEvidenceRecords = 50

func (s *Service) evidence(ctx context.Context, res *match.Result) (func(int) ([]byte, error), error) {
	var ledgerIDs, lineIDs []string
	for i := range res.Outcomes {
		o := &res.Outcomes[i]
		if !o.Class.IsDiscrepancy() {
			continue
		}
		for k, j := range o.Ledger {
			if k < maxEvidenceRecords {
				ledgerIDs = append(ledgerIDs, res.In.Ledger[j].ID)
			}
		}
		for _, j := range append(append([]int32(nil), o.Lines...), o.Context...) {
			lineIDs = append(lineIDs, res.In.Lines[j].ID)
		}
	}
	full, err := s.store.LedgerDetails(ctx, ledgerIDs)
	if err != nil {
		return nil, err
	}
	raw, err := s.store.LineRaw(ctx, lineIDs)
	if err != nil {
		return nil, err
	}
	line := func(j int32) domain.SettlementLine {
		l := res.In.Lines[j]
		l.Raw = raw[l.ID]
		return l
	}
	return func(i int) ([]byte, error) {
		o := &res.Outcomes[i]
		ev := Evidence{Class: o.Class, Rule: o.Rule, Detail: o.Detail, Expected: o.Expected, Actual: o.Actual, Deadline: o.Deadline, SettledOn: o.SettledOn,
			Ledger: []domain.LedgerTxn{}, Lines: []domain.SettlementLine{}}
		seen := map[domain.PaymentKey]bool{}
		addPayment := func(provider, ref string) {
			k := domain.PaymentKey{Provider: provider, Ref: ref}
			if p, ok := res.In.Payments[k]; ok && !seen[k] && len(ev.Payments) < maxEvidenceRecords {
				seen[k] = true
				ev.Payments = append(ev.Payments, p)
			}
		}
		for k, j := range o.Ledger {
			if k >= maxEvidenceRecords {
				ev.Truncated = len(o.Ledger) - maxEvidenceRecords
				break
			}
			t := res.In.Ledger[j]
			if f, ok := full[t.ID]; ok {
				t = f
			}
			ev.Ledger = append(ev.Ledger, t)
			addPayment(t.Provider, t.Reference)
		}
		for _, j := range o.Lines {
			ev.Lines = append(ev.Lines, line(j))
			if res.In.Lines[j].Kind == domain.LineItem {
				addPayment(res.In.Lines[j].Provider, res.In.Lines[j].Reference)
			}
		}
		for _, j := range o.Context {
			ev.Context = append(ev.Context, line(j))
		}
		return json.Marshal(ev)
	}, nil
}
