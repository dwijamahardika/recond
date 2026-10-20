package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

type world struct {
	dir   string
	svc   *app.Service
	store *sqlite.Store
	ds    *gen.Dataset
	files []gen.SettlementFile
}

func newWorld(t *testing.T, cfg gen.Config) *world {
	t.Helper()
	rules, err := config.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Rules = rules
	ds, err := gen.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files, err := ds.WriteDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(dir, "recond.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &world{dir: dir, svc: app.NewService(store, rules, clock.NewFake(ds.Truth.AsOf), nil, nil), store: store, ds: ds, files: files}
}

func (w *world) ingestAll(t *testing.T) []app.IngestResult {
	t.Helper()
	ctx := context.Background()
	out := make([]app.IngestResult, 0, len(w.files)+2)
	r, err := w.svc.IngestLedger(ctx, app.FileSource(filepath.Join(w.dir, gen.LedgerFile)), "", "")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, r)
	for _, f := range w.files {
		r, err := w.svc.IngestSettlement(ctx, f.Provider, app.FileSource(filepath.Join(w.dir, f.Name)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	r, err = w.svc.IngestWebhooks(ctx, app.FileSource(filepath.Join(w.dir, gen.EventsFile)))
	if err != nil {
		t.Fatal(err)
	}
	return append(out, r)
}

// The ground-truth property again, this time through files on disk, SQLite,
// the stored run and the CSV report a finance user would get.
func TestEndToEndGroundTruth(t *testing.T) {
	w := newWorld(t, gen.Config{Seed: 7, Payments: 4000, FaultRate: 0.1, BatchFaultRate: 0.8})
	ctx := context.Background()
	for _, r := range w.ingestAll(t) {
		if r.AlreadyIngested || r.Stats.Rejected > 0 {
			t.Fatalf("unexpected ingest result %+v", r)
		}
	}
	sum, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := w.svc.WriteItemsCSV(ctx, sum.RunID, &buf); err != nil {
		t.Fatal(err)
	}
	scored, err := gen.ScoredFromItemsCSV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if score := gen.ScoreOutcomes(&w.ds.Truth, scored); !score.Perfect() {
		t.Fatalf("ground truth violated through storage: %s", score)
	}
	for _, c := range sum.Classes {
		if c.Class.IsDiscrepancy() && c.Outcomes != w.ds.Truth.Expected[c.Class] {
			t.Errorf("%s: %d outcomes, truth expects %d", c.Class, c.Outcomes, w.ds.Truth.Expected[c.Class])
		}
	}
	if sum.Exceptions.Opened != sum.Discrepancies || sum.Discrepancies == 0 {
		t.Fatalf("exceptions opened %d for %d discrepancies", sum.Exceptions.Opened, sum.Discrepancies)
	}

	// Payment states in the store fold to the generator's intent.
	for k, want := range w.ds.Truth.FinalStatus {
		p, _, err := w.svc.Payment(ctx, k.Provider, k.Ref)
		if err != nil || p.Status() != want {
			t.Fatalf("%v: %s, want %s (%v)", k, p.Status(), want, err)
		}
	}

	// Re-ingesting everything is a no-op.
	for _, r := range w.ingestAll(t) {
		if !r.AlreadyIngested {
			t.Fatalf("re-ingest not idempotent: %+v", r)
		}
	}

	// The run reproduces from its recorded watermarks and rules.
	v, err := w.svc.Verify(ctx, sum.RunID)
	if err != nil || !v.Reproduced {
		t.Fatalf("verify: %+v %v", v, err)
	}

	// A second run over the same data changes nothing in the queue.
	sum2, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	if sum2.ResultDigest != sum.ResultDigest || sum2.Exceptions.Opened != 0 || sum2.Exceptions.StillOpen != sum.Discrepancies {
		t.Fatalf("second run: %+v", sum2.Exceptions)
	}
}

// Runs are versioned: data ingested after a run does not change what that
// run reproduces to, while a new run sees it - and the queue follows.
func TestRunsAreVersionedAndQueueFollows(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 3, Payments: 300, FaultRate: -1, BatchFaultRate: -1, Days: 2})
	// Ingest ledger and webhooks, but only the first settlement file.
	if _, err := w.svc.IngestLedger(ctx, app.FileSource(filepath.Join(w.dir, gen.LedgerFile)), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.IngestWebhooks(ctx, app.FileSource(filepath.Join(w.dir, gen.EventsFile))); err != nil {
		t.Fatal(err)
	}
	first := w.files[0]
	if _, err := w.svc.IngestSettlement(ctx, first.Provider, app.FileSource(filepath.Join(w.dir, first.Name))); err != nil {
		t.Fatal(err)
	}
	run1, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	if run1.Discrepancies == 0 {
		t.Fatal("expected missing settlements with most files absent")
	}
	for _, f := range w.files[1:] {
		if _, err := w.svc.IngestSettlement(ctx, f.Provider, app.FileSource(filepath.Join(w.dir, f.Name))); err != nil {
			t.Fatal(err)
		}
	}
	run2, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	if run2.Discrepancies != 0 || run2.Exceptions.AutoClosed != run1.Discrepancies {
		t.Fatalf("run2: %d discrepancies, auto-closed %d of %d", run2.Discrepancies, run2.Exceptions.AutoClosed, run1.Discrepancies)
	}
	for _, id := range []int64{run1.RunID, run2.RunID} {
		v, err := w.svc.Verify(ctx, id)
		if err != nil || !v.Reproduced {
			t.Fatalf("run %d does not reproduce: %+v %v", id, v, err)
		}
	}
	// Auto-closed exceptions keep their history.
	list, err := w.svc.Exceptions(ctx, sqlite.ExceptionFilter{Status: sqlite.StatusAutoClosed, Limit: 1})
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	d, err := w.svc.Exception(ctx, list[0].ID)
	if err != nil || len(d.History) != 2 || d.History[0].Action != "opened" || d.History[1].Action != "auto_closed" {
		t.Fatalf("history %+v %v", d, err)
	}
}

func TestExceptionWorkflow(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 5, Payments: 500, FaultRate: 0.2, BatchFaultRate: -1})
	w.ingestAll(t)
	if _, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf}); err != nil {
		t.Fatal(err)
	}
	open, err := w.svc.Exceptions(ctx, sqlite.ExceptionFilter{Status: sqlite.StatusOpen, Limit: 5})
	if err != nil || len(open) == 0 {
		t.Fatalf("%v %v", open, err)
	}
	id := open[0].ID
	d, err := w.svc.Exception(ctx, id)
	if err != nil || len(d.Evidence) == 0 {
		t.Fatalf("evidence missing: %v", err)
	}
	var ev app.Evidence
	if err := json.Unmarshal(d.Evidence, &ev); err != nil || (len(ev.Ledger) == 0 && len(ev.Lines) == 0) {
		t.Fatalf("evidence %s %v", d.Evidence, err)
	}
	for _, l := range ev.Lines {
		if l.Raw == "" {
			t.Fatal("settlement evidence must carry the raw source row")
		}
	}

	if _, err := w.svc.Resolve(ctx, id, "", "x"); !errors.Is(err, app.ErrValidation) {
		t.Fatal("actor is required")
	}
	if _, err := w.svc.Resolve(ctx, id, "recond", "x"); !errors.Is(err, app.ErrValidation) {
		t.Fatal("system actor is reserved")
	}
	if _, err := w.svc.Resolve(ctx, id, "ops-1", "  "); !errors.Is(err, app.ErrValidation) {
		t.Fatal("reason is required")
	}
	if _, err := w.svc.Reopen(ctx, id, "ops-1", "nothing to reopen"); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatalf("reopen of open exception must conflict, got %v", err)
	}
	if e, err := w.svc.Resolve(ctx, id, "ops-1", "provider confirmed; adjustment in next cycle"); err != nil || e.Status != sqlite.StatusResolved {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := w.svc.Resolve(ctx, id, "ops-2", "again"); !errors.Is(err, sqlite.ErrConflict) {
		t.Fatal("double resolve must conflict")
	}
	// A rerun does not undo a human decision.
	sum, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil || sum.Exceptions.Resolved != 1 {
		t.Fatalf("%+v %v", sum.Exceptions, err)
	}
	if _, err := w.svc.Reopen(ctx, id, "ops-2", "adjustment never arrived"); err != nil {
		t.Fatal(err)
	}
	d, _ = w.svc.Exception(ctx, id)
	actions := make([]string, 0, len(d.History))
	for _, h := range d.History {
		actions = append(actions, h.Action+":"+h.Actor)
	}
	if got := strings.Join(actions, ","); got != "opened:recond,resolved:ops-1,reopened:ops-2" {
		t.Fatalf("audit trail %s", got)
	}
}

func TestReportFilesAndFormulaSafety(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 11, Payments: 200})
	w.ingestAll(t)
	// A hostile reference in a provider file must not become a spreadsheet formula.
	evil := filepath.Join(w.dir, "evil.csv")
	src := "settlement_id,merchant_ref,transaction_time,settlement_date,currency,gross_amount,fee_amount,net_amount\n" +
		"EVIL-1,\"=HYPERLINK(\"\"x\"\")\",2026-10-01T03:00:00Z,2026-10-03,IDR,100.00,1.50,98.50\n"
	if err := os.WriteFile(evil, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := w.svc.IngestSettlement(ctx, "psp_a", app.FileSource(evil)); err != nil || r.Stats.Inserted != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	sum, err := w.svc.Run(ctx, app.RunOptions{AsOf: w.ds.Truth.AsOf})
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := w.svc.WriteReport(ctx, sum.RunID, filepath.Join(w.dir, "reports"))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := os.ReadFile(files.Items)
	if !bytes.Contains(items, []byte(`'=HYPERLINK`)) || bytes.Contains(items, []byte(`,=HYPERLINK`)) {
		t.Fatal("formula not neutralised")
	}
	if _, err := os.Stat(files.Summary); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.IngestSettlement(ctx, "psp_zz", app.FileSource(evil)); !errors.Is(err, app.ErrUnknownProvider) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestApplyWebhook(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, gen.Config{Seed: 1, Payments: 10})
	body := []byte(`{"event_id":"evt_1","payment_ref":"ORD-X","status":"paid","amount":1000,"currency":"IDR","sequence":2,"occurred_at":"2026-10-01T03:00:00Z"}`)
	r, err := w.svc.ApplyWebhook(ctx, "psp_a", body)
	if err != nil || r.Verdict != domain.Applied || r.After != domain.StatusPaid {
		t.Fatalf("%+v %v", r, err)
	}
	if r, _ := w.svc.ApplyWebhook(ctx, "psp_a", body); !r.Duplicate {
		t.Fatal("duplicate not detected")
	}
	late := []byte(`{"event_id":"evt_0","payment_ref":"ORD-X","status":"pending","amount":1000,"currency":"IDR","sequence":1,"occurred_at":"2026-10-01T02:59:00Z"}`)
	if r, _ := w.svc.ApplyWebhook(ctx, "psp_a", late); !r.OutOfOrder || r.After != domain.StatusPaid {
		t.Fatalf("late pending must not overwrite paid: %+v", r)
	}
	if _, err := w.svc.ApplyWebhook(ctx, "psp_a", []byte(`{"event_id":"e"}`)); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("malformed: %v", err)
	}
	if _, err := w.svc.ApplyWebhook(ctx, "nope", body); !errors.Is(err, app.ErrUnknownProvider) {
		t.Fatalf("unknown provider: %v", err)
	}
}
