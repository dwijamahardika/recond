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
}

func summarize(res *match.Result) Summary {
	byClass := map[domain.Class]*ClassStat{}
	for _, c := range domain.Classes {
		byClass[c] = &ClassStat{Class: c}
	}
	prov := map[string]*ProviderStat{}
	p := func(name string) *ProviderStat {
		if prov[name] == nil {
			prov[name] = &ProviderStat{Provider: name}
		}
		return prov[name]
	}
	for _, t := range res.In.Ledger {
		p(t.Provider).LedgerTxns++
	}
	for _, l := range res.In.Lines {
		p(l.Provider).Lines++
	}
	sum := Summary{LedgerTxns: len(res.In.Ledger), Lines: len(res.In.Lines), Payments: len(res.In.Payments), Outcomes: len(res.Outcomes)}
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
	for _, c := range domain.Classes {
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
}

// Run reconciles everything ingested so far and stores the result.
func (s *Service) Run(ctx context.Context, opt RunOptions) (*Summary, error) {
	t0 := s.clock.Now()
	if opt.AsOf.IsZero() {
		opt.AsOf = t0
	}
	timings := map[string]int64{}
	mark := func(name string, since time.Time) time.Time {
		now := time.Now()
		timings[name] = now.Sub(since).Milliseconds()
		return now
	}
	start := time.Now()
	in, err := s.store.LoadInputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load inputs: %w", err)
	}
	step := mark("load", start)
	res, err := match.Reconcile(s.rules, &match.Input{AsOf: opt.AsOf.UTC(), Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments})
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
	summaryJSON, _ := json.Marshal(sum)
	run := sqlite.Run{
		AsOf: opt.AsOf.UTC(), EngineVersion: match.EngineVersion, ConfigJSON: string(cfgJSON), ConfigSHA256: sum.ConfigSHA256,
		MaxFileID: in.MaxFileID, MaxEventSeq: in.MaxEventSeq, InputDigest: in.Digest, ResultDigest: digest,
		LedgerCount: len(in.Ledger), LineCount: len(in.Lines), PaymentCount: len(in.Payments),
		SummaryJSON: string(summaryJSON), DurationMS: time.Since(start).Milliseconds(),
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
	s.log.Info("run complete", "run_id", runID, "ledger", len(in.Ledger), "lines", len(in.Lines), "discrepancies", sum.Discrepancies,
		"opened", sync.Opened, "auto_closed", sync.AutoClosed, "duration_ms", timings["total"], "digest", digest)
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
	in, err := s.store.LoadInputsAt(ctx, run.MaxFileID, run.MaxEventSeq)
	if err != nil {
		return nil, err
	}
	res, err := match.Reconcile(cfg, &match.Input{AsOf: run.AsOf, Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments})
	if err != nil {
		return nil, err
	}
	v := &Verification{
		RunID: run.ID, EngineVersion: run.EngineVersion, CurrentEngine: match.EngineVersion,
		InputDigest: run.InputDigest, InputMatches: in.Digest == run.InputDigest,
		ResultDigest: run.ResultDigest, ReplayDigest: res.Digest(),
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
