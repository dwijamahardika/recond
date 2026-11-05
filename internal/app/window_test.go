package app_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/match"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

// windowedSvc is the same store with the feature flag on.
func (w *world) windowedSvc(on bool) *app.Service {
	rules := w.ds.Rules
	return app.NewService(w.store, rules, clock.NewFake(w.ds.Truth.AsOf), nil, nil, app.WithWindowedRuns(on))
}

// days returns consecutive windows of size days covering every record a
// generated world can contain.
func days(ds *gen.Dataset, size int, cfg gen.Config) []match.Window {
	start := domain.DateOf(cfg.Start, ds.Rules.Providers[0].Location())
	if cfg.Start.IsZero() {
		start = domain.CivilDate(2026, 10, 1)
	}
	var out []match.Window
	for from := start.AddDays(-1); from <= start.AddDays(cfg.Days+1); from = from.AddDays(size) {
		out = append(out, match.Window{From: from, To: from.AddDays(size - 1)})
	}
	return out
}

// itemRows returns a run's report rows without run and outcome ids, sorted:
// what a finance user would compare across runs.
func itemRows(t *testing.T, svc *app.Service, runID int64) []string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := svc.WriteItemsCSV(context.Background(), runID, &buf); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(recs)-1)
	for _, r := range recs[1:] {
		out = append(out, strings.Join(r[2:], "\x1f"))
	}
	sort.Strings(out)
	return out
}

func TestWindowedRunsRequireFlag(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 1, Payments: 50})
	win := &match.Window{From: domain.CivilDate(2026, 10, 1), To: domain.CivilDate(2026, 10, 1)}
	if _, err := w.svc.Run(ctx, app.RunOptions{Window: win}); !errors.Is(err, app.ErrWindowedRunsDisabled) {
		t.Fatalf("flag off: %v", err)
	}
	if _, err := w.windowedSvc(true).Run(ctx, app.RunOptions{Window: &match.Window{From: win.From, To: win.From.AddDays(-1)}}); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("invalid window must be a validation error: %v", err)
	}
	sum, err := w.windowedSvc(true).Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf, Window: win})
	if err != nil || sum.Window == nil || sum.Window.Lookbehind != 1 || sum.Window.Lookahead != 1 || sum.Window.Days != 1 {
		t.Fatalf("defaults: %+v %v", sum, err)
	}
	p := sum.Window.Providers
	if len(p) != 2 || p[0].Provider != "bank_b" || !p[0].From.Equal(time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)) ||
		!p[1].From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !p[1].To.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("per-provider bounds (bank_b's 1 Oct starts at 17:00 UTC on 30 Sep): %+v", p)
	}
}

// Consecutive windows through SQLite give the finance user the same report
// rows as one full run, every windowed run verifies, and the exception
// queue sees each discrepancy once.
func TestWindowedRunsThroughStorage(t *testing.T) {
	ctx := context.Background()
	cfg := gen.Config{Seed: 7, Payments: 3000, Days: 5, FaultRate: 0.1, BatchFaultRate: 0.8, BoundaryRate: 0.05}
	w := newWorld(t, cfg)
	w.ingestAll(t)
	full, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	want := itemRows(t, w.svc, full.RunID)
	svc := w.windowedSvc(true)
	var got []string
	var scored []gen.Scored
	ledger, stillOpen := 0, 0
	for _, win := range days(w.ds, 2, cfg) {
		sum, err := svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf, Window: &win})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Window.OutsideWindow != 0 || sum.Exceptions.Opened != 0 || sum.Exceptions.AutoClosed != 0 {
			t.Fatalf("window %s: %+v %+v", win, sum.Window, sum.Exceptions)
		}
		ledger += sum.LedgerTxns
		stillOpen += sum.Exceptions.StillOpen
		for _, p := range sum.Providers {
			if p.MatchRate > 1 || p.Matched+p.Flagged > p.LedgerTxns {
				t.Fatalf("window %s: provider stats beyond 100%%: %+v", win, p)
			}
		}
		got = append(got, itemRows(t, svc, sum.RunID)...)
		var buf bytes.Buffer
		if _, err := svc.WriteItemsCSV(ctx, sum.RunID, &buf); err != nil {
			t.Fatal(err)
		}
		s, err := gen.ScoredFromItemsCSV(&buf)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range s {
			o.ID += len(scored)
			scored = append(scored, o)
		}
		v, err := w.svc.Verify(ctx, sum.RunID) // the flag-off service: verify never needs the flag
		if err != nil || !v.Reproduced || v.Window == nil || *v.Window != sum.Window.Window {
			t.Fatalf("window %s does not verify: %+v %v", win, v, err)
		}
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("report rows of the windows (%d) differ from the full run's (%d)", len(got), len(want))
	}
	if ledger != full.LedgerTxns || stillOpen != full.Discrepancies {
		t.Fatalf("in-window ledger %d vs %d; still open %d vs %d discrepancies", ledger, full.LedgerTxns, stillOpen, full.Discrepancies)
	}
	if score := gen.ScoreOutcomes(&w.ds.Truth, scored); !score.Perfect() {
		t.Fatalf("ground truth over the windows' reports: %s", score)
	}
}

// Runs made before the feature was switched on keep verifying, and their
// stored summaries are untouched; switching the flag off again does not
// strand windowed runs.
func TestPreexistingRunsVerifyAfterWindowedRuns(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 4, Payments: 800, FaultRate: 0.1})
	w.ingestAll(t)
	old, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := w.svc.Summary(ctx, old.RunID)
	if err != nil {
		t.Fatal(err)
	}
	win := &match.Window{From: domain.CivilDate(2026, 10, 1), To: domain.CivilDate(2026, 10, 2)}
	newer, err := w.windowedSvc(true).Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf, Window: win})
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range []*app.Service{w.svc, w.windowedSvc(true), w.windowedSvc(false)} {
		for _, id := range []int64{old.RunID, newer.RunID} {
			if v, err := svc.Verify(ctx, id); err != nil || !v.Reproduced {
				t.Fatalf("run %d (flag %v): %+v %v", id, svc.WindowedRunsEnabled(), v, err)
			}
		}
	}
	_, after, err := w.svc.Summary(ctx, old.RunID)
	if err != nil || after.SummaryJSON != before.SummaryJSON || after.Window != nil || strings.Contains(after.SummaryJSON, `"window"`) {
		t.Fatalf("pre-existing run changed: %v", err)
	}
}

// Settlements arrive after a full run opened missing_in_settlement for
// them. Each windowed run closes exactly its own window's exceptions; all
// of them together close every one, once.
func TestWindowedRunScopesExceptionQueue(t *testing.T) {
	ctx := context.Background()
	cfg := gen.Config{Seed: 3, Payments: 600, FaultRate: -1, BatchFaultRate: -1, Days: 4}
	w := newWorld(t, cfg)
	if _, err := w.svc.IngestLedger(ctx, app.FileSource(filepath.Join(w.dir, gen.LedgerFile)), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.IngestWebhooks(ctx, app.FileSource(filepath.Join(w.dir, gen.EventsFile))); err != nil {
		t.Fatal(err)
	}
	first, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil || first.Discrepancies == 0 {
		t.Fatalf("%+v %v", first, err)
	}
	for _, f := range w.files {
		if _, err := w.svc.IngestSettlement(ctx, f.Provider, app.FileSource(filepath.Join(w.dir, f.Name))); err != nil {
			t.Fatal(err)
		}
	}
	svc := w.windowedSvc(true)
	closed := 0
	for i, win := range days(w.ds, 1, cfg) {
		sum, err := svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf, Window: &win})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Discrepancies != 0 {
			t.Fatalf("window %s: %d discrepancies in a clean world", win, sum.Discrepancies)
		}
		closed += sum.Exceptions.AutoClosed
		if i == 2 && (closed == 0 || closed >= first.Discrepancies) {
			t.Fatalf("after three one-day windows %d of %d exceptions closed: each window must close only its own", closed, first.Discrepancies)
		}
	}
	if closed != first.Discrepancies {
		t.Fatalf("windows closed %d exceptions, the full run opened %d", closed, first.Discrepancies)
	}
	if last, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf}); err != nil || last.Exceptions.AutoClosed != 0 || last.Discrepancies != 0 {
		t.Fatalf("nothing left for a full run to close: %+v %v", last, err)
	}
}
