package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

func rules(t *testing.T) *match.Config {
	t.Helper()
	c := &match.Config{Providers: []match.ProviderRules{
		{Name: "psp_a", Format: "psp_a", Timezone: "+00:00", SettlementDays: 2, FeeToleranceMinor: 1,
			Fees: map[domain.Currency]domain.FeeRule{"IDR": {RateBPS: 150}}},
		{Name: "bank_b", Format: "bank_b", Timezone: "+07:00", SettlementDays: 1,
			Fees: map[domain.Currency]domain.FeeRule{"IDR": {RateBPS: 90, FixedMinor: 20_000}}},
	}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func oct(day int, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }

func txn(id, ref string, at time.Time) domain.LedgerTxn {
	return domain.LedgerTxn{ID: id, Provider: "psp_a", Reference: ref, Amount: 1_000_000, Currency: "IDR", CreatedAt: at, FromAccount: "a", ToAccount: "b"}
}

// line is a psp_a item; a zero at writes it without a transaction time.
func line(id, ref string, at time.Time, settled domain.Date) domain.SettlementLine {
	fee := domain.FeeRule{RateBPS: 150}.Fee(1_000_000)
	return domain.SettlementLine{ID: "psp_a:" + id, Provider: "psp_a", Kind: domain.LineItem, Reference: ref, Currency: "IDR",
		Gross: 1_000_000, Fee: fee, Net: 1_000_000 - fee, FeeKnown: true, TxnAt: at, SettledOn: settled, SourceLine: 2, Raw: id}
}

func date(day int) domain.Date { return domain.CivilDate(2026, time.October, day) }

// file ingests one file of rows and returns its id.
func file(t *testing.T, s *Store, sha string, ledger []domain.LedgerTxn, lines []domain.SettlementLine) int64 {
	t.Helper()
	kind := "ledger"
	if len(lines) > 0 {
		kind = "settlement"
	}
	st, err := s.Ingest(context.Background(), FileMeta{Kind: kind, Source: "x", Name: sha, SHA256: sha}, func(in *Ingester) error {
		for _, r := range ledger {
			if err := in.Ledger(r, r.ID); err != nil {
				return err
			}
		}
		for _, l := range lines {
			if err := in.Line(l, l.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return st.FileID
}

func TestProberNormalisesReferences(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	first := file(t, s, "f1", []domain.LedgerTxn{txn("t1", "  ord-1 ", oct(3, 9)), txn("t2", "ORD-2", oct(3, 9))}, nil)
	payout := domain.SettlementLine{ID: "bank_b:PO", Provider: "bank_b", Kind: domain.LinePayout, Reference: "ORD-1", Currency: "IDR",
		Gross: 100, Net: 100, SettledOn: date(4), CoversFrom: oct(3, 0), CoversTo: oct(3, 6), ItemCount: 1, SourceLine: 2, Raw: "po"}
	file(t, s, "f2", nil, []domain.SettlementLine{line("S1", "ORD-1", oct(3, 9), date(5)), payout})
	file(t, s, "f3", []domain.LedgerTxn{txn("t3", "Ord-1", oct(4, 9))}, nil) // after the watermark below

	p := s.Prober(ctx, first+1)
	led, err := p.LedgerByRef("psp_a", []string{"ORD-1", "ORD-2", "ORD-9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(led["ORD-1"]) != 1 || led["ORD-1"][0].ID != "t1" || len(led["ORD-2"]) != 1 || len(led["ORD-9"]) != 0 {
		t.Fatalf("ledger by ref (case and spaces folded, file watermark respected): %+v", led)
	}
	lines, err := p.LinesByRef("psp_a", []string{"ORD-1"})
	if err != nil || len(lines["ORD-1"]) != 1 || lines["ORD-1"][0].ID != "psp_a:S1" || lines["ORD-1"][0].TxnAt.IsZero() {
		t.Fatalf("lines by ref must return items only, fully read: %+v %v", lines, err)
	}
	for at, want := range map[time.Time]int{oct(3, 0): 1, oct(3, 5): 1, oct(3, 6): 0, oct(2, 23): 0} {
		got, err := p.PayoutsCovering("bank_b", at)
		if err != nil || len(got) != want {
			t.Errorf("payouts covering %s: %d (%v), want %d", at, len(got), err, want)
		}
	}
	if all, _ := s.Prober(ctx, 99).LedgerByRef("psp_a", []string{"ORD-1"}); len(all["ORD-1"]) != 2 {
		t.Fatalf("a later watermark sees the later file: %+v", all)
	}
	// match.ProbeKey and SQLite's upper(trim(x)) agree.
	for _, ref := range []string{"  ord-1 ", "Ord-1", "ORD-1"} {
		var key string
		if err := s.db.QueryRowContext(ctx, `SELECT upper(trim(?))`, ref).Scan(&key); err != nil || key != match.ProbeKey(ref) {
			t.Fatalf("%q: sqlite %q, ProbeKey %q", ref, key, match.ProbeKey(ref))
		}
	}
}

func TestRunWindowRoundTrip(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	w := &match.Window{From: date(1), To: date(7), Lookbehind: 1, Lookahead: 3}
	res := &match.Result{In: &match.Input{}}
	full, _, err := s.SaveRun(ctx, Run{EngineVersion: "1", SummaryJSON: "{}"}, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	win, _, err := s.SaveRun(ctx, Run{EngineVersion: "1", SummaryJSON: "{}", Window: w}, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.Run(ctx, full); err != nil || r.Window != nil {
		t.Fatalf("full run: %+v %v", r.Window, err)
	}
	if r, err := s.Run(ctx, win); err != nil || r.Window == nil || *r.Window != *w {
		t.Fatalf("windowed run: %+v %v", r.Window, err)
	}
	if list, err := s.Runs(ctx, 10); err != nil || len(list) != 2 || list[0].Window == nil || list[1].Window != nil {
		t.Fatalf("runs: %+v %v", list, err)
	}
}

// reconcile runs the engine over everything stored, optionally windowed.
func reconcile(t *testing.T, s *Store, cfg *match.Config, w *match.Window) *match.Result {
	t.Helper()
	ctx := context.Background()
	in, err := s.LoadInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mi := &match.Input{AsOf: oct(30, 0), Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments}
	if w != nil {
		mi.Window, mi.Prober = w, s.Prober(ctx, in.MaxFileID)
	}
	res, err := match.Reconcile(cfg, mi)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func save(t *testing.T, s *Store, res *match.Result) ExceptionSync {
	t.Helper()
	r := Run{EngineVersion: "1", SummaryJSON: "{}"}
	if res.Window != nil {
		r.Window = &res.Window.Window
	}
	_, sync, err := s.SaveRun(context.Background(), r, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sync
}

func statuses(t *testing.T, s *Store) map[string]string {
	t.Helper()
	list, err := s.Exceptions(context.Background(), ExceptionFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range list {
		out[string(e.Class)+":"+e.Refs] = e.Status
	}
	return out
}

// A windowed run auto-closes only open exceptions dated inside its window:
// not another window's, and never an undated one from before the migration.
func TestWindowedSyncScopesAutoClose(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	cfg := rules(t)
	file(t, s, "l1", []domain.LedgerTxn{txn("t1", "ORD-1", oct(2, 9)), txn("t2", "ORD-2", oct(9, 9))}, nil)
	if sync := save(t, s, reconcile(t, s, cfg, nil)); sync.Opened != 2 {
		t.Fatalf("full run: %+v", sync)
	}
	// An open, undated exception: written by the previous release and
	// reopened by a human after the migrate-phase full run.
	if _, err := s.db.ExecContext(ctx, prevInsertException); err != nil {
		t.Fatal(err)
	}
	var dated int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM exceptions WHERE window_date IN ('2026-10-02', '2026-10-09')`).Scan(&dated); err != nil || dated != 2 {
		t.Fatalf("a full run dates the exceptions it opens: %d %v", dated, err)
	}
	// Both settlements arrive; a run over 1-7 Oct sees only t1's.
	file(t, s, "s1", nil, []domain.SettlementLine{line("S1", "ORD-1", oct(2, 9), date(4)), line("S2", "ORD-2", oct(9, 9), date(11))})
	w := &match.Window{From: date(1), To: date(7), Lookbehind: 1, Lookahead: 1}
	sync := save(t, s, reconcile(t, s, cfg, w))
	if sync.AutoClosed != 1 || sync.Opened != 0 {
		t.Fatalf("windowed run must close only its own window's exception: %+v", sync)
	}
	want := map[string]string{"missing_in_settlement:ORD-1": "auto_closed", "missing_in_settlement:ORD-2": "open", "missing_in_settlement:ORD-t1": "open"}
	if got := statuses(t, s); len(got) != 3 || got["missing_in_settlement:ORD-1"] != want["missing_in_settlement:ORD-1"] ||
		got["missing_in_settlement:ORD-2"] != "open" || got["missing_in_settlement:ORD-t1"] != "open" {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	// A full run keeps its old semantics: everything it does not see closes.
	if sync := save(t, s, reconcile(t, s, cfg, nil)); sync.AutoClosed != 2 {
		t.Fatalf("full run: %+v", sync)
	}
}

// Records a windowed run deferred as outside_window have no verdict yet:
// their open exception stays open, until a run that can decide closes it.
func TestWindowedSyncKeepsDeferredOpen(t *testing.T) {
	s, _ := open(t)
	cfg := rules(t)
	file(t, s, "l1", []domain.LedgerTxn{txn("t1", "ORD-1", oct(7, 9))}, nil)
	if sync := save(t, s, reconcile(t, s, cfg, nil)); sync.Opened != 1 {
		t.Fatalf("full run: %+v", sync)
	}
	// The settlement arrives late and without a transaction time: dated
	// settled_on - 2 = 11 Oct, beyond a 1-day lookahead from 7 Oct.
	file(t, s, "s1", nil, []domain.SettlementLine{line("S1", "ORD-1", time.Time{}, date(13))})
	w := &match.Window{From: date(1), To: date(7), Lookbehind: 1, Lookahead: 1}
	res := reconcile(t, s, cfg, w)
	if len(res.Outcomes) != 1 || res.Outcomes[0].Class != domain.ClassOutsideWindow {
		t.Fatalf("want one outside_window group, got %+v", res.Outcomes)
	}
	if sync := save(t, s, res); sync.AutoClosed != 0 || sync.KeptDeferred != 1 {
		t.Fatalf("a deferred record's exception must stay open: %+v", sync)
	}
	// A lookahead that reaches decides it: late, and the old exception closes.
	w.Lookahead = 5
	if sync := save(t, s, reconcile(t, s, cfg, w)); sync.AutoClosed != 1 || sync.Opened != 1 {
		t.Fatalf("decided: %+v", sync)
	}
	if got := statuses(t, s); got["missing_in_settlement:ORD-1"] != "auto_closed" || got["late_settlement:ORD-1"] != "open" {
		t.Fatalf("statuses %v", got)
	}
}

// The window loader returns a superset of the visible range, so the
// engine over it equals the engine over everything; the replay loader
// (event-log fold) agrees with the projection.
func TestWindowLoadMatchesFullLoad(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	cfg := rules(t)
	var ledger []domain.LedgerTxn
	var lines []domain.SettlementLine
	for d := 1; d <= 20; d++ {
		for h := 0; h < 24; h += 5 {
			ref := "ORD-" + date(d).String() + "-" + string(rune('A'+h))
			ledger = append(ledger, txn("t"+ref, ref, oct(d, h)))
			switch {
			case d%4 == 0: // untimed and late
				lines = append(lines, line("S"+ref, ref, time.Time{}, date(d).AddDays(2+d%3)))
			case h != 10:
				lines = append(lines, line("S"+ref, ref, oct(d, h), date(d).AddDays(2)))
			}
		}
	}
	file(t, s, "l", ledger, nil)
	file(t, s, "s", nil, lines)
	full, err := s.LoadInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []match.Window{
		{From: date(5), To: date(11), Lookbehind: 1, Lookahead: 1},
		{From: date(1), To: date(1), Lookbehind: 1, Lookahead: 4},
		{From: date(18), To: date(25), Lookbehind: 3, Lookahead: 1},
	} {
		mem, err := match.Reconcile(cfg, &match.Input{AsOf: oct(30, 0), Ledger: full.Ledger, Lines: full.Lines, Payments: full.Payments,
			Window: &w, Prober: match.NewMemoryProber(full.Ledger, full.Lines)})
		if err != nil {
			t.Fatal(err)
		}
		for name, load := range map[string]func() (*Inputs, error){
			"current": func() (*Inputs, error) { return s.LoadWindow(ctx, w.Bounds(cfg)) },
			"replay":  func() (*Inputs, error) { return s.LoadWindowAt(ctx, full.MaxFileID, full.MaxEventSeq, w.Bounds(cfg)) },
		} {
			in, err := load()
			if err != nil {
				t.Fatal(err)
			}
			if in.Digest != full.Digest || len(in.Ledger) >= len(full.Ledger) {
				t.Fatalf("%s %s: digest %s vs %s, %d of %d ledger rows", name, w, in.Digest, full.Digest, len(in.Ledger), len(full.Ledger))
			}
			res, err := match.Reconcile(cfg, &match.Input{AsOf: oct(30, 0), Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments,
				Window: &w, Prober: s.Prober(ctx, in.MaxFileID)})
			if err != nil {
				t.Fatal(err)
			}
			if res.Digest() != mem.Digest() || len(res.Outcomes) == 0 {
				t.Fatalf("%s %s: storage path %s (%d groups) != in-memory %s (%d)", name, w, res.Digest(), len(res.Outcomes), mem.Digest(), len(mem.Outcomes))
			}
		}
	}
}

// Without the 0002 indexes a window would scan whole tables; pin the plans.
func TestWindowQueriesUseIndexes(t *testing.T) {
	s, _ := open(t)
	for q, want := range map[string]string{
		`SELECT ` + ledgerCols + ` FROM ledger_txns WHERE created_at >= 1 AND created_at < 2 AND file_id <= 3`:                                                             "USING INDEX ledger_txns_created",
		`SELECT ` + lineCols + ` FROM settlement_lines WHERE coalesce(txn_at, covers_from) >= 1 AND coalesce(txn_at, covers_from) < 2 AND file_id <= 3`:                    "USING INDEX settlement_lines_window",
		`SELECT ` + lineCols + ` FROM settlement_lines WHERE txn_at IS NULL AND covers_from IS NULL AND settled_on BETWEEN '2026-10-01' AND '2026-10-05' AND file_id <= 3`: "USING INDEX settlement_lines_untimed",
		`SELECT id FROM ledger_txns WHERE provider = 'p' AND upper(trim(reference)) IN ('A','B') AND file_id <= 3`:                                                         "USING INDEX ledger_txns_ref",
		`SELECT id FROM settlement_lines WHERE provider = 'p' AND kind = 'item' AND upper(trim(reference)) IN ('A','B') AND file_id <= 3`:                                  "USING INDEX settlement_lines_item_ref",
		`SELECT id FROM settlement_lines WHERE provider = 'p' AND kind = 'payout' AND covers_from <= 5 AND covers_from >= 1 AND covers_to > 5 AND file_id <= 3`:            "USING INDEX settlement_lines_payout",
		`SELECT ` + paymentCols + ` FROM payments WHERE (provider, payment_ref) IN (VALUES ('a','b'), ('c','d'))`:                                                          "USING PRIMARY KEY",
		`SELECT ` + eventCols + ` FROM webhook_events WHERE seq <= 9 AND (provider, payment_ref) IN (VALUES ('a','b'), ('c','d')) ORDER BY provider, payment_ref, seq`:     "USING INDEX webhook_events_payment",
	} {
		plan := queryPlan(t, s, q)
		if !strings.Contains(strings.Join(plan, "\n"), want) {
			t.Errorf("%s\nplan %v, want %s", q, plan, want)
		}
	}
}

func queryPlan(t *testing.T, s *Store, q string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return plan
}
