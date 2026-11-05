package gen_test

import (
	"testing"

	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/match"
)

// TestUnwindowedDigestsUnchanged pins full-history runs to the result
// digests the previous release produced (captured on main at b6230fc, before
// windowed runs existed). Windowing must not change a single byte of an
// unwindowed result: same outcomes, same order, same details.
func TestUnwindowedDigestsUnchanged(t *testing.T) {
	golden := map[int64]string{
		1: "d9e8ee519f6b604728aa942fbcfef6ad5a37bb06976fc74fc2b9b38672616230",
		2: "efe31e63a46f336d3c0f5049a7b7fb17da11bbe76ab3e6ef7286ee253b0ab3fb",
		3: "53d2c89d6875e763b80644dc5196ea93a745b10b004fab52180780a5e06f893c",
	}
	for seed, want := range golden {
		ds := generate(t, gen.Config{Seed: seed, Payments: 2000, Days: 4, FaultRate: 0.12, BatchFaultRate: 0.8, BoundaryRate: 0.05, DupEventRate: 0.3})
		in, _ := parseBack(t, ds)
		res, err := match.Reconcile(ds.Rules, in)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Digest(); got != want {
			t.Errorf("seed %d: digest %s, previous release %s", seed, got, want)
		}
		if res.Window != nil {
			t.Fatal("a full run has no window")
		}
	}
}
