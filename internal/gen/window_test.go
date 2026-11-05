package gen_test

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/match"
)

// The windowed-runs properties (docs/lifecycle/windowed-runs/03-test-plan.md).
// Windows are run in memory over the parsed-back world, with a counterpart
// prober that knows every record, exactly as storage would answer.

// span returns the earliest and latest window date of any record.
func span(t *testing.T, rules *match.Config, in *match.Input) (domain.Date, domain.Date) {
	t.Helper()
	var lo, hi domain.Date
	see := func(d domain.Date) {
		if lo == 0 || d < lo {
			lo = d
		}
		hi = max(hi, d)
	}
	for i := range in.Ledger {
		r, _ := rules.Provider(in.Ledger[i].Provider)
		see(match.LedgerDate(&in.Ledger[i], r))
	}
	for i := range in.Lines {
		r, _ := rules.Provider(in.Lines[i].Provider)
		see(match.LineDate(&in.Lines[i], r))
	}
	return lo, hi
}

// windowed runs consecutive windows of size days over every record's date
// and returns each run's result.
func windowed(t *testing.T, ds *gen.Dataset, in *match.Input, size, lookbehind, lookahead int) []*match.Result {
	t.Helper()
	lo, hi := span(t, ds.Rules, in)
	prober := match.NewMemoryProber(in.Ledger, in.Lines)
	var out []*match.Result
	for from := lo; from <= hi; from = from.AddDays(size) {
		w := match.Window{From: from, To: from.AddDays(size - 1), Lookbehind: lookbehind, Lookahead: lookahead}
		res, err := match.Reconcile(ds.Rules, &match.Input{AsOf: in.AsOf, Ledger: in.Ledger, Lines: in.Lines, Payments: in.Payments,
			Window: &w, Prober: prober})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
	}
	return out
}

// members identifies a group by its records.
func members(res *match.Result, o *match.Outcome) string {
	ids := make([]string, 0, len(o.Ledger)+len(o.Lines))
	for _, i := range o.Ledger {
		ids = append(ids, "L:"+res.In.Ledger[i].ID)
	}
	for _, i := range o.Lines {
		ids = append(ids, "S:"+res.In.Lines[i].ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, " ")
}

// canon renders everything the result digest covers.
func canon(res *match.Result, o *match.Outcome) string {
	ctx := make([]string, 0, len(o.Context))
	for _, i := range o.Context {
		ctx = append(ctx, res.In.Lines[i].ID)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|ctx=%v|exp=%+v|act=%+v|%s|%s|%s", members(res, o), o.Class, o.Rule, o.Provider, o.Currency,
		ctx, o.Expected, o.Actual, o.Deadline, o.SettledOn, o.Detail)
}

// scoredAll converts several results for one scoring pass, with outcome IDs
// unique across them.
func scoredAll(results []*match.Result) []gen.Scored {
	var out []gen.Scored
	for _, res := range results {
		for _, s := range Scored(res) {
			s.ID += len(out) + 1
			out = append(out, s)
		}
	}
	return out
}

// TestWindowedRunsPartitionFullRun is G1: with default margins, consecutive
// windows of any size report exactly the full run's groups, each once, with
// identical classes, amounts, dates and details, and nothing deferred. The
// generator places payments milliseconds before cut-offs, so ledger rows
// booked after midnight for payments taken before it cross every boundary.
func TestWindowedRunsPartitionFullRun(t *testing.T) {
	seeds, payments := 8, 3000
	if testing.Short() {
		seeds, payments = 2, 1500
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			ds := generate(t, gen.Config{Seed: seed, Payments: payments, Days: 8, FaultRate: 0.12, BatchFaultRate: 0.8, BoundaryRate: 0.05, DupEventRate: 0.3})
			in, _ := parseBack(t, ds)
			full, err := match.Reconcile(ds.Rules, in)
			if err != nil {
				t.Fatal(err)
			}
			want := make([]string, 0, len(full.Outcomes))
			for i := range full.Outcomes {
				want = append(want, canon(full, &full.Outcomes[i]))
			}
			sort.Strings(want)
			for _, size := range []int{1, 2, 3, 7} {
				results := windowed(t, ds, in, size, 1, 1)
				var got []string
				notOwned := 0
				for _, res := range results {
					if res.Window.Deferred != 0 {
						t.Fatalf("size %d: %d outside_window groups with default margins", size, res.Window.Deferred)
					}
					notOwned += res.Window.NotOwned
					for i := range res.Outcomes {
						got = append(got, canon(res, &res.Outcomes[i]))
					}
				}
				sort.Strings(got)
				if diff := firstDiff(want, got); diff != "" {
					t.Fatalf("size %d: windows differ from the full run (%d vs %d groups): %s", size, len(got), len(want), diff)
				}
				if size == 1 && notOwned == 0 {
					t.Fatal("one-day windows must compute some groups for their neighbours")
				}
				score := gen.ScoreOutcomes(&ds.Truth, scoredAll(results))
				if !score.Perfect() {
					t.Fatalf("size %d: ground truth over the union of windows: %s", size, score)
				}
				t.Logf("size %d: %d windows, %d groups reported, %d computed for neighbours, %d faults", size, len(results), len(got), notOwned, score.Faults)
			}
		})
	}
}

// TestWindowedRunsDeferWhenMarginTooSmall is G2. bank_b late item credits
// without a transaction time are dated by settlement, 1-5 days after their
// transaction. With margins smaller than that, every full-run group must
// still appear exactly once across the windows - unchanged, or as
// outside_window with the same records - and nothing may be flagged that
// the truth does not explain or counted twice. With margins covering the
// spread, the windows reproduce the full run exactly.
func TestWindowedRunsDeferWhenMarginTooSmall(t *testing.T) {
	seeds := 6
	if testing.Short() {
		seeds = 2
	}
	configs := map[string]gen.Config{
		"item_delay only": {Payments: 2000, Days: 6, BatchFaultRate: 1, FaultRate: -1, UntimedItemRate: 1,
			Faults: map[string]bool{gen.FaultItemDelay: true}},
		"all faults": {Payments: 2000, Days: 6, BatchFaultRate: 1, FaultRate: 0.12, BoundaryRate: 0.05, UntimedItemRate: 1},
	}
	for name, base := range configs {
		for seed := int64(1); seed <= int64(seeds); seed++ {
			t.Run(fmt.Sprintf("%s/seed=%d", name, seed), func(t *testing.T) {
				t.Parallel()
				cfg := base
				cfg.Seed = seed
				ds := generate(t, cfg)
				in, _ := parseBack(t, ds)
				full, err := match.Reconcile(ds.Rules, in)
				if err != nil {
					t.Fatal(err)
				}
				if s := gen.ScoreOutcomes(&ds.Truth, Scored(full)); !s.Perfect() {
					t.Fatalf("untimed credits must not change the full run: %s", s)
				}
				byMembers := map[string]string{}
				for i := range full.Outcomes {
					o := &full.Outcomes[i]
					byMembers[members(full, o)] = canon(full, o)
				}
				for _, m := range [][2]int{{1, 1}, {1, 3}, {3, 1}} {
					for _, size := range []int{1, 3} {
						results := windowed(t, ds, in, size, m[0], m[1])
						remaining := make(map[string]string, len(byMembers))
						for k, v := range byMembers {
							remaining[k] = v
						}
						deferred := 0
						for _, res := range results {
							for i := range res.Outcomes {
								o := &res.Outcomes[i]
								key := members(res, o)
								want, ok := remaining[key]
								if !ok {
									t.Fatalf("margins %v size %d: group %s is not a full-run group, or is reported twice: %s", m, size, key, canon(res, o))
								}
								delete(remaining, key)
								if o.Class == domain.ClassOutsideWindow {
									deferred++
									continue
								}
								if got := canon(res, o); got != want {
									t.Fatalf("margins %v size %d: group changed\n got %s\nwant %s", m, size, got, want)
								}
							}
						}
						for k := range remaining {
							t.Fatalf("margins %v size %d: group %s reported by no window", m, size, k)
						}
						s := gen.ScoreOutcomes(&ds.Truth, scoredAll(results))
						if len(s.Unexplained) > 0 || len(s.Duplicated) > 0 || len(s.Missed) > 0 || len(s.Mispaired) > 0 {
							t.Fatalf("margins %v size %d: %s", m, size, s)
						}
						if size == 1 && m == [2]int{1, 1} && (deferred == 0 || len(s.Deferred) == 0) {
							t.Fatalf("1-day windows with 1-day margins must defer some untimed late credits: %s", s)
						}
						t.Logf("margins %v size %d: %d windows, %d groups deferred, %d of %d faults detected, %d deferred", m, size, len(results), deferred, s.Detected, s.Faults, len(s.Deferred))
					}
				}
				// Margins covering the largest spread (5 days): exact again.
				var got, want []string
				for _, res := range windowed(t, ds, in, 1, 5, 5) {
					for i := range res.Outcomes {
						got = append(got, canon(res, &res.Outcomes[i]))
					}
				}
				for _, v := range byMembers {
					want = append(want, v)
				}
				sort.Strings(got)
				sort.Strings(want)
				if diff := firstDiff(want, got); diff != "" {
					t.Fatalf("5-day margins must reproduce the full run: %s", diff)
				}
			})
		}
	}
}

func TestScoreCountsDuplicatesAndDeferrals(t *testing.T) {
	truth := &gen.Truth{Faults: []gen.Fault{
		{Kind: gen.FaultDelay, Class: domain.ClassLateSettlement, Key: "S1", LedgerID: "L1", LineID: "S1"},
		{Kind: gen.FaultDelay, Class: domain.ClassLateSettlement, Key: "S2", LedgerID: "L2", LineID: "S2"},
	}}
	s := gen.ScoreOutcomes(truth, []gen.Scored{
		{ID: 1, Class: domain.ClassLateSettlement, Ledger: []string{"L1"}, Lines: []string{"S1"}},
		{ID: 2, Class: domain.ClassLateSettlement, Ledger: []string{"L1"}, Lines: []string{"S1"}}, // counted twice
		{ID: 3, Class: domain.ClassOutsideWindow, Ledger: []string{"L2"}, Lines: []string{"S2"}},
	})
	if s.Detected != 1 || len(s.Duplicated) != 1 || len(s.Deferred) != 1 || s.DeferredOutcomes != 1 || len(s.Unexplained) != 0 || len(s.Missed) != 0 || s.Perfect() {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(s.String(), "duplicated 1; deferred 1") {
		t.Fatal(s.String())
	}
}

func firstDiff(want, got []string) string {
	for i := 0; i < max(len(want), len(got)); i++ {
		var w, g string
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		if w != g {
			return "at " + strconv.Itoa(i) + "\n want " + w + "\n  got " + g
		}
	}
	return ""
}
