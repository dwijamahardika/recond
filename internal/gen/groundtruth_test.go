package gen_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/ingest"
	"github.com/dwijamahardika/recond/internal/match"
)

// TestGroundTruth is the property the whole design is accountable to: for
// random worlds with injected faults, rendered to provider files, parsed back
// through the adapters, with webhooks duplicated and delivered out of order,
// reconciliation must classify every injected fault exactly, flag nothing
// else, and pair every matched transaction with the line that settled it.
func TestGroundTruth(t *testing.T) {
	seeds, payments := 24, 3000
	if testing.Short() {
		seeds, payments = 4, 1500
	}
	kinds := map[string]int{}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			ds := generate(t, gen.Config{Seed: seed, Payments: payments, Days: 3, FaultRate: 0.12, BatchFaultRate: 0.8, BoundaryRate: 0.05, DupEventRate: 0.3})
			in, book := parseBack(t, ds)

			// Webhooks: every delivery order folds to the intended status.
			for k, want := range ds.Truth.FinalStatus {
				p, ok := in.Payments[k]
				if !ok || p.Status() != want {
					t.Fatalf("payment %v: status %s, want %s (%+v)", k, p.Status(), want, p)
				}
				if folded, _ := domain.Fold(book.History(k)); folded != p {
					t.Fatalf("payment %v: incremental state differs from fold:\n%+v\n%+v", k, p, folded)
				}
			}

			res, err := match.Reconcile(ds.Rules, in)
			if err != nil {
				t.Fatal(err)
			}
			score := gen.ScoreOutcomes(&ds.Truth, Scored(res))
			if !score.Perfect() {
				t.Fatalf("ground truth violated: %s", score)
			}
			if score.Paired < len(in.Ledger)/2 {
				t.Fatalf("only %d pairs checked", score.Paired)
			}
			again, _ := match.Reconcile(ds.Rules, in)
			if again.Digest() != res.Digest() {
				t.Fatal("same input, different digest")
			}
		})
	}
	// Coverage: every fault kind must actually have been exercised.
	for seed := int64(1); seed <= int64(seeds); seed++ {
		ds := generate(t, gen.Config{Seed: seed, Payments: payments, Days: 3, FaultRate: 0.12, BatchFaultRate: 0.8, BoundaryRate: 0.05, DupEventRate: 0.3})
		for _, f := range ds.Truth.Faults {
			kinds[f.Kind]++
		}
	}
	for _, k := range append(append([]string(nil), gen.ItemFaults...), gen.BatchFaults...) {
		if kinds[k] == 0 {
			t.Errorf("fault kind %s never injected", k)
		}
	}
	t.Logf("faults injected across seeds: %v", kinds)
}

// TestGroundTruthCleanWorld: with no faults, everything matches.
func TestGroundTruthCleanWorld(t *testing.T) {
	ds := generate(t, gen.Config{Seed: 99, Payments: 2000, FaultRate: -1, BatchFaultRate: -1})
	in, _ := parseBack(t, ds)
	res, err := match.Reconcile(ds.Rules, in)
	if err != nil {
		t.Fatal(err)
	}
	for c, n := range res.Counts() {
		if c != domain.ClassMatched && n > 0 {
			t.Errorf("%d %s outcomes in a clean world", n, c)
		}
	}
	if len(ds.Truth.Faults) != 0 {
		t.Fatalf("faults injected with rates disabled: %d", len(ds.Truth.Faults))
	}
}

func generate(t *testing.T, cfg gen.Config) *gen.Dataset {
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
	return ds
}

type memFile struct{ bytes.Buffer }

func (*memFile) Close() error { return nil }

// parseBack renders the dataset to files and reads them through the real
// adapters, so the formats are part of what the property covers.
func parseBack(t *testing.T, ds *gen.Dataset) (*match.Input, *domain.Book) {
	t.Helper()
	files := map[string]*memFile{}
	settlement, err := ds.Render(func(name string) (io.WriteCloser, error) {
		f := &memFile{}
		files[name] = f
		return f, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reject := func(r ingest.Reject) { t.Fatalf("generated row rejected: %+v", r) }
	in := &match.Input{AsOf: ds.Truth.AsOf}
	if err := ingest.ReadLedger(bytes.NewReader(files[gen.LedgerFile].Bytes()), "csv", ingest.Options{}, func(l domain.LedgerTxn, _ string) error {
		in.Ledger = append(in.Ledger, l)
		return nil
	}, reject); err != nil {
		t.Fatal(err)
	}
	for _, sf := range settlement {
		rules, _ := ds.Rules.Provider(sf.Provider)
		a, _ := ingest.AdapterFor(rules.Format)
		if err := a.Read(bytes.NewReader(files[sf.Name].Bytes()), ingest.Options{Provider: sf.Provider, Location: rules.Location()},
			func(l domain.SettlementLine, _ string) error {
				in.Lines = append(in.Lines, l)
				return nil
			}, reject); err != nil {
			t.Fatal(err)
		}
	}
	book := domain.NewBook()
	if err := ingest.ReadEvents(bytes.NewReader(files[gen.EventsFile].Bytes()), ingest.Options{}, func(e domain.PaymentEvent, _ string) error {
		book.Apply(e)
		return nil
	}, reject); err != nil {
		t.Fatal(err)
	}
	in.Payments = book.Payments()
	return in, book
}

// Scored converts a result for the scorer.
func Scored(res *match.Result) []gen.Scored {
	out := make([]gen.Scored, len(res.Outcomes))
	for i, o := range res.Outcomes {
		s := gen.Scored{ID: i + 1, Class: o.Class, Rule: o.Rule}
		for _, j := range o.Ledger {
			s.Ledger = append(s.Ledger, res.In.Ledger[j].ID)
		}
		for _, j := range o.Lines {
			s.Lines = append(s.Lines, res.In.Lines[j].ID)
		}
		out[i] = s
	}
	return out
}
