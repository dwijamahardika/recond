package match_test

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/ingest"
	"github.com/dwijamahardika/recond/internal/match"
)

// BenchmarkReconcile measures the engine alone (no I/O) on a generated
// world with the default fault mix.
func BenchmarkReconcile(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(byN(n), func(b *testing.B) {
			rules, err := config.LoadRules("")
			if err != nil {
				b.Fatal(err)
			}
			ds, err := gen.Generate(gen.Config{Seed: 1, Payments: n, Days: 7, Rules: rules})
			if err != nil {
				b.Fatal(err)
			}
			in := input(b, ds)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := match.Reconcile(ds.Rules, in); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(in.Ledger)*b.N)/b.Elapsed().Seconds(), "txns/s")
		})
	}
}

func byN(n int) string { return "n=" + strconv.Itoa(n) }

type memFile struct{ bytes.Buffer }

func (*memFile) Close() error { return nil }

func input(b *testing.B, ds *gen.Dataset) *match.Input {
	b.Helper()
	files := map[string]*memFile{}
	settlement, err := ds.Render(func(name string) (io.WriteCloser, error) {
		f := &memFile{}
		files[name] = f
		return f, nil
	})
	if err != nil {
		b.Fatal(err)
	}
	in := &match.Input{AsOf: ds.Truth.AsOf, Ledger: ds.Ledger}
	for _, sf := range settlement {
		r, _ := ds.Rules.Provider(sf.Provider)
		a, _ := ingest.AdapterFor(r.Format)
		_ = a.Read(bytes.NewReader(files[sf.Name].Bytes()), ingest.Options{Provider: sf.Provider, Location: r.Location()},
			func(l domain.SettlementLine, _ string) error { in.Lines = append(in.Lines, l); return nil }, func(ingest.Reject) {})
	}
	book := domain.NewBook()
	for _, e := range ds.Events {
		book.Apply(e)
	}
	in.Payments = book.Payments()
	return in
}
