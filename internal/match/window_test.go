package match

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York for the DST case, independent of the host

	"github.com/dwijamahardika/recond/internal/domain"
)

// win is the week of 1-7 October 2026 with the default 1-day margins.
func win(from, to int) *Window {
	return &Window{From: d(from), To: d(to), Lookbehind: 1, Lookahead: 1}
}

func runW(t *testing.T, in *Input, w *Window) *Result {
	t.Helper()
	if in.AsOf.IsZero() {
		in.AsOf = day0.AddDate(0, 0, 30)
	}
	in.Window, in.Prober = w, NewMemoryProber(in.Ledger, in.Lines)
	res, err := Reconcile(testConfig(t), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// itemB is a bank_b itemised credit; at zero writes it without a
// transaction time, as the format allows.
func itemB(id, ref string, gross int64, at time.Time, settled domain.Date) domain.SettlementLine {
	return domain.SettlementLine{ID: "bank_b:" + id, Provider: "bank_b", Kind: domain.LineItem, Reference: ref, Currency: "IDR",
		Gross: gross, Fee: feeB(gross), Net: netB(gross), FeeKnown: true, TxnAt: at, SettledOn: settled}
}

func TestWindowValidate(t *testing.T) {
	ok := Window{From: d(0), To: d(6), Lookbehind: 1, Lookahead: 1}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := (Window{From: d(0), To: d(6)}).WithDefaults(); got != ok {
		t.Fatalf("defaults: %+v", got)
	}
	for name, w := range map[string]Window{
		"no dates":         {Lookbehind: 1, Lookahead: 1},
		"to before from":   {From: d(6), To: d(0), Lookbehind: 1, Lookahead: 1},
		"too long":         {From: d(0), To: d(366), Lookbehind: 1, Lookahead: 1},
		"zero lookbehind":  {From: d(0), To: d(6), Lookbehind: 0, Lookahead: 1},
		"margin too large": {From: d(0), To: d(6), Lookbehind: 1, Lookahead: 32},
	} {
		if err := w.Validate(); !errors.Is(err, ErrWindow) {
			t.Errorf("%s: want ErrWindow, got %v", name, err)
		}
	}
	if (Window{From: d(0), To: d(0)}).Days() != 1 || ok.Days() != 7 {
		t.Fatal("days")
	}
}

func TestWindowDates(t *testing.T) {
	cfg := testConfig(t)
	ny := &Config{Providers: []ProviderRules{{Name: "psp_ny", Format: "psp_a", Timezone: "America/New_York", SettlementDays: 2,
		Fees: map[domain.Currency]domain.FeeRule{"USD": {RateBPS: 100}}}}}
	if err := ny.Validate(); err != nil {
		t.Fatal(err)
	}
	a, _ := cfg.Provider("psp_a")
	b, _ := cfg.Provider("bank_b")
	n, _ := ny.Provider("psp_ny")
	date := domain.CivilDate
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name string
		got  domain.Date
		want domain.Date
	}{
		{"psp_a (UTC) last millisecond of the day", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-10-07T23:59:59.999Z")}, a), date(2026, 10, 7)},
		{"psp_a (UTC) midnight starts the next day", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-10-08T00:00:00Z")}, a), date(2026, 10, 8)},
		{"bank_b (UTC+7) 23:59:59.999 local", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-10-07T16:59:59.999Z")}, b), date(2026, 10, 7)},
		{"bank_b (UTC+7) local midnight is 17:00 UTC", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-10-07T17:00:00Z")}, b), date(2026, 10, 8)},
		{"late settlement keeps its transaction date", LineDate(&domain.SettlementLine{TxnAt: ts("2026-10-07T22:00:00Z"), SettledOn: date(2026, 10, 14)}, a), date(2026, 10, 7)},
		{"untimed line: settled_on minus T+N", LineDate(&domain.SettlementLine{SettledOn: date(2026, 10, 9)}, b), date(2026, 10, 8)},
		{"payout: covered period start, local", LineDate(&domain.SettlementLine{Kind: domain.LinePayout, CoversFrom: ts("2026-10-07T11:00:00Z"), CoversTo: ts("2026-10-07T17:00:00Z"), SettledOn: date(2026, 10, 8)}, b), date(2026, 10, 7)},
		{"payout at local midnight belongs to that day", LineDate(&domain.SettlementLine{Kind: domain.LinePayout, CoversFrom: ts("2026-10-07T17:00:00Z"), SettledOn: date(2026, 10, 9)}, b), date(2026, 10, 8)},
		// 1 November 2026: US clocks fall back, the day has 25 hours.
		{"DST: 23:59:59 EDT on 31 Oct", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-11-01T03:59:59Z")}, n), date(2026, 10, 31)},
		{"DST: 23:30 EST on 1 Nov (hour 25)", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-11-02T04:30:00Z")}, n), date(2026, 11, 1)},
		{"DST: midnight EST on 2 Nov", LedgerDate(&domain.LedgerTxn{CreatedAt: ts("2026-11-02T05:00:00Z")}, n), date(2026, 11, 2)},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

// Storage selects by Bounds and the engine filters exactly, so every
// instant whose window date is visible for some provider must fall inside
// the bounds.
func TestWindowBoundsCoverVisibleRange(t *testing.T) {
	cfg := testConfig(t)
	w := Window{From: d(0), To: d(6), Lookbehind: 1, Lookahead: 2}
	b := w.Bounds(cfg)
	if want := time.Date(2026, 9, 30, 0, 0, 0, 0, wib); !b.From.Equal(want) {
		t.Errorf("from %s, want %s (UTC+7 midnight is the earliest)", b.From, want)
	}
	if want := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC); !b.To.Equal(want) {
		t.Errorf("to %s, want %s (UTC midnight is the latest)", b.To, want)
	}
	if b.UntimedFrom != w.VisibleFrom().AddDays(1) || b.UntimedTo != w.VisibleTo().AddDays(2) {
		t.Errorf("untimed bounds %s..%s", b.UntimedFrom, b.UntimedTo)
	}
	for _, name := range []string{"psp_a", "bank_b"} {
		r, _ := cfg.Provider(name)
		for at := b.From.Add(-48 * time.Hour); at.Before(b.To.Add(48 * time.Hour)); at = at.Add(17 * time.Minute) {
			visible := w.Visible(LedgerDate(&domain.LedgerTxn{CreatedAt: at}, r))
			inside := !at.Before(b.From) && at.Before(b.To)
			if visible && !inside {
				t.Fatalf("%s %s is visible but outside the load bounds", name, at)
			}
		}
		for s := w.VisibleFrom().AddDays(-5); s <= w.VisibleTo().AddDays(5); s++ {
			l := domain.SettlementLine{SettledOn: s}
			if w.Visible(LineDate(&l, r)) && (s < b.UntimedFrom || s > b.UntimedTo) {
				t.Fatalf("%s untimed line settled %s is visible but outside the load bounds", name, s)
			}
		}
	}
}

func TestWindowOwnership(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	payout := func(id, fromLocal string, count int, gross int64, settled domain.Date) domain.SettlementLine {
		from, err := time.ParseInLocation("2006-01-02T15:04", fromLocal, wib)
		if err != nil {
			t.Fatal(err)
		}
		return payoutB(id, from, count, netB(gross), feeB(gross), settled)
	}
	cases := []struct {
		name   string
		ledger []domain.LedgerTxn
		lines  []domain.SettlementLine
		w      *Window
		want   []expect
	}{
		{"pair inside the window",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-03T10:00:00Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-03T10:00:00Z"), d(4), -1)}, win(0, 6),
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"late settlement after the window end keeps its transaction's window",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-07T10:00:00Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-07T10:00:00Z"), d(11), -1)}, win(0, 6),
			[]expect{{"late_settlement", "reference", "t1", "psp_a:S1"}}},
		{"last millisecond of to is owned",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-07T23:59:59.999Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-07T23:59:59.999Z"), d(8), -1)}, win(0, 6),
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"first instant after to belongs to the next window",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-08T00:00:00Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-08T00:00:00Z"), d(9), -1)}, win(0, 6),
			nil},
		{"booked after midnight, paid before: the earlier day reports it",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-08T00:00:01Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-07T23:59:59.5Z"), d(8), -1)}, win(0, 6),
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"... and the next window, which sees both, leaves it",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-08T00:00:01Z"))},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-07T23:59:59.5Z"), d(8), -1)}, win(7, 13),
			nil},
		{"a group entirely in the lookbehind margin is not reported",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-09-30T12:00:00Z"))}, nil, win(0, 6),
			nil},
		{"no counterpart anywhere: still missing_in_settlement",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-05T12:00:00Z"))}, nil, win(0, 6),
			[]expect{{"missing_in_settlement", "unmatched", "t1", ""}}},
		{"bank_b: 22:00 local on the 7th is in the week",
			[]domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, at("2026-10-07T15:00:00Z"))},
			[]domain.SettlementLine{payout("PO", "2026-10-07T18:00", 1, 1_000_000, d(7))}, win(0, 6),
			[]expect{{"matched", "batch", "t1", "bank_b:PO"}}},
		{"bank_b: local midnight (17:00 UTC) starts the next week",
			[]domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, at("2026-10-07T17:30:00Z"))},
			[]domain.SettlementLine{payout("PO", "2026-10-08T00:00", 1, 1_000_000, d(8))}, win(0, 6),
			nil},
		{"payout short by one: the excluded transaction stays missing (its payout is visible)",
			[]domain.LedgerTxn{
				led("t1", "bank_b", "ORD-1", 1_000_000, at("2026-10-03T00:00:00Z")),
				led("t2", "bank_b", "ORD-2", 2_500_000, at("2026-10-03T01:00:00Z"))},
			[]domain.SettlementLine{payout("PO", "2026-10-03T06:00", 1, 1_000_000, d(3))}, win(0, 6),
			[]expect{{"matched", "batch", "t1", "bank_b:PO"}, {"missing_in_settlement", "unmatched", "t2", ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runW(t, &Input{Ledger: tc.ledger, Lines: tc.lines}, tc.w)
			assertOutcomes(t, res, tc.want...)
		})
	}
}

func TestOutsideWindow(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// A bank_b transaction on 7 Oct whose itemised credit came late and
	// without a transaction time: dated settled_on - 1 = 11 Oct.
	lateUntimed := func() *Input {
		return &Input{
			Ledger: []domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, at("2026-10-07T03:00:00Z"))},
			Lines:  []domain.SettlementLine{itemB("L1", "ORD-1", 1_000_000, time.Time{}, d(11))},
		}
	}
	t.Run("counterpart beyond the lookahead is attached and deferred", func(t *testing.T) {
		res := runW(t, lateUntimed(), win(0, 6))
		assertOutcomes(t, res, expect{"outside_window", "reference", "t1", "bank_b:L1"})
		o := res.Outcomes[0]
		for _, want := range []string{"bank_b:L1", "dated 2026-10-11", "2026-09-30..2026-10-08", "(attached)", "pairs by reference", "-lookahead"} {
			if !strings.Contains(o.Detail, want) {
				t.Errorf("detail %q lacks %q", o.Detail, want)
			}
		}
		if o.Class.IsDiscrepancy() || res.Window.Deferred != 1 || res.Window.Scope(res.Window.LineDate[o.Lines[0]]) != "outside" {
			t.Fatalf("deferral must be informational and the line attached from outside: %+v %+v", o, res.Window)
		}
	})
	t.Run("the later windows do not report it again", func(t *testing.T) {
		assertOutcomes(t, runW(t, lateUntimed(), win(7, 13)))  // 8-14 Oct sees both; the pair is dated 7 Oct
		assertOutcomes(t, runW(t, lateUntimed(), win(9, 15)))  // 10-16 Oct sees only the line: counterpart earlier
		assertOutcomes(t, runW(t, lateUntimed(), win(10, 16))) // 11-17 Oct: same
	})
	t.Run("a lookahead that reaches decides it", func(t *testing.T) {
		w := win(0, 6)
		w.Lookahead = 4
		assertOutcomes(t, runW(t, lateUntimed(), w), expect{"late_settlement", "reference", "t1", "bank_b:L1"})
	})
	t.Run("counterpart beyond the lookbehind: the earlier window reports the pair", func(t *testing.T) {
		// Booked six days after the provider's transaction (a manual posting).
		in := func() *Input {
			return &Input{
				Ledger: []domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-09T09:00:00Z"))},
				Lines:  []domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at("2026-10-03T09:00:00Z"), d(4), -1)},
			}
		}
		assertOutcomes(t, runW(t, in(), win(7, 13)))
		res := runW(t, in(), win(0, 6))
		assertOutcomes(t, res, expect{"outside_window", "reference", "t1", "psp_a:S1"})
		if !strings.Contains(res.Outcomes[0].Detail, "ledger transaction t1 dated 2026-10-09") {
			t.Fatal(res.Outcomes[0].Detail)
		}
	})
	t.Run("ambiguous reference: deferred alone, never dropped", func(t *testing.T) {
		in := &Input{
			Ledger: []domain.LedgerTxn{
				led("t0", "psp_a", "ORD-1", 1_000_000, at("2026-09-25T09:00:00Z")),
				led("t1", "psp_a", "ORD-1", 1_000_000, at("2026-10-05T09:00:00Z"))},
			Lines: []domain.SettlementLine{itemA("S0", "ORD-1", 1_000_000, at("2026-09-25T09:00:00Z"), d(-4), -1)},
		}
		res := runW(t, in, win(0, 6))
		assertOutcomes(t, res, expect{"outside_window", "unmatched", "t1", ""})
		if d := res.Outcomes[0].Detail; !strings.Contains(d, "not attached") || !strings.Contains(d, "would be missing_in_settlement") {
			t.Fatal(res.Outcomes[0].Detail)
		}
	})
	t.Run("payout counterpart: deferred alone", func(t *testing.T) {
		paid := time.Date(2026, 10, 7, 10, 0, 0, 0, wib)
		in := &Input{
			Ledger: []domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, time.Date(2026, 10, 9, 10, 0, 0, 0, wib))},
			Lines:  []domain.SettlementLine{payoutB("PO", time.Date(2026, 10, 7, 6, 0, 0, 0, wib), 1, netB(1_000_000), feeB(1_000_000), d(7))},
			Payments: map[domain.PaymentKey]domain.Payment{
				{Provider: "bank_b", Ref: "ORD-1"}: {Provider: "bank_b", Ref: "ORD-1", Lifecycle: domain.StatusPaid, PaidAt: paid}},
		}
		res := runW(t, in, win(8, 14)) // 9-15 Oct: the payout (7 Oct) is two days back
		assertOutcomes(t, res, expect{"outside_window", "unmatched", "t1", ""})
		if !strings.Contains(res.Outcomes[0].Detail, "payout bank_b:PO") {
			t.Fatal(res.Outcomes[0].Detail)
		}
	})
	t.Run("line without a ledger transaction anywhere: still missing_in_ledger", func(t *testing.T) {
		in := &Input{Lines: []domain.SettlementLine{itemA("S1", "ORD-X", 1_000_000, at("2026-10-03T09:00:00Z"), d(4), -1)}}
		assertOutcomes(t, runW(t, in, win(0, 6)), expect{"missing_in_ledger", "unmatched", "", "psp_a:S1"})
	})
	t.Run("attaching never writes into the caller's slices", func(t *testing.T) {
		// Everything given is visible, so the engine works on the caller's
		// slices directly (the storage path); the attached line comes from
		// the prober and must not land in the caller's spare capacity.
		all := lateUntimed()
		lines := make([]domain.SettlementLine, 1, 4)
		lines[0] = itemA("S9", "ORD-9", 500_000, at("2026-10-05T09:00:00Z"), d(6), -1)
		in := &Input{AsOf: day0.AddDate(0, 0, 30), Ledger: all.Ledger, Lines: lines, Window: win(0, 6),
			Prober: NewMemoryProber(all.Ledger, append(all.Lines, lines[0]))}
		res, err := Reconcile(testConfig(t), in)
		if err != nil {
			t.Fatal(err)
		}
		assertOutcomes(t, res, expect{"outside_window", "reference", "t1", "bank_b:L1"}, expect{"missing_in_ledger", "unmatched", "", "psp_a:S9"})
		if spare := lines[:2][1]; spare.ID != "" || len(in.Lines) != 1 {
			t.Fatalf("caller's slice was written: %+v", spare)
		}
	})
}

// Known limit (RFC section 6, G3): a pair matched on amount and time, with a
// mangled reference, has nothing to look up. Spread wider than the margin,
// the window holding the line calls it missing. Pinned so that a fix shows
// up here and in the docs.
func TestKnownLimitToleranceSpreadBeyondMargin(t *testing.T) {
	paid := time.Date(2026, 10, 7, 23, 58, 0, 0, time.UTC)
	in := func() *Input {
		return &Input{
			Ledger: []domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, paid.Add(26*time.Hour))}, // booked on the 9th
			Lines:  []domain.SettlementLine{itemA("S1", "", 1_000_000, paid, d(8), -1)},
			Payments: map[domain.PaymentKey]domain.Payment{
				{Provider: "psp_a", Ref: "ORD-1"}: {Provider: "psp_a", Ref: "ORD-1", Lifecycle: domain.StatusPaid, PaidAt: paid}},
		}
	}
	assertOutcomes(t, run(t, in()), expect{"matched", "tolerance", "t1", "psp_a:S1"})
	assertOutcomes(t, runW(t, in(), win(0, 6)), expect{"missing_in_ledger", "unmatched", "", "psp_a:S1"})
	w := win(0, 6)
	w.Lookahead = 2
	assertOutcomes(t, runW(t, in(), w), expect{"matched", "tolerance", "t1", "psp_a:S1"})
}

func TestWindowNeedsProberAndValidWindow(t *testing.T) {
	cfg := testConfig(t)
	if _, err := Reconcile(cfg, &Input{AsOf: day0, Window: win(0, 6)}); err == nil || !strings.Contains(err.Error(), "prober") {
		t.Fatalf("want prober error, got %v", err)
	}
	bad := &Window{From: d(0), To: d(6)}
	if _, err := Reconcile(cfg, &Input{AsOf: day0, Window: bad, Prober: NewMemoryProber(nil, nil)}); !errors.Is(err, ErrWindow) {
		t.Fatalf("want ErrWindow, got %v", err)
	}
	if _, err := Reconcile(cfg, &Input{AsOf: day0, Window: win(0, 6), Prober: NewMemoryProber(nil, nil),
		Ledger: []domain.LedgerTxn{led("t1", "psp_z", "R", 1, day0)}}); err == nil || !strings.Contains(err.Error(), "psp_z") {
		t.Fatalf("unknown provider must be a config error in a window too, got %v", err)
	}
}

func TestProbeKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ORD-1", "ORD-1"}, {"  ord-1 ", "ORD-1"}, {"Ord-é", "ORD-é"}, {"\tord", "\tORD"}, {"", ""},
	} {
		if got := ProbeKey(tc.in); got != tc.want {
			t.Errorf("ProbeKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A full run's anchor dates are the same as a window computes them.
func TestAnchorDate(t *testing.T) {
	ledger := []domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, time.Date(2026, 10, 8, 0, 0, 1, 0, wib))}
	lines := []domain.SettlementLine{itemB("L1", "ORD-1", 1_000_000, time.Date(2026, 10, 7, 23, 59, 59, 0, wib), d(8))}
	full := run(t, &Input{Ledger: ledger, Lines: lines})
	w := runW(t, &Input{Ledger: ledger, Lines: lines}, win(0, 6))
	if a, b := full.AnchorDate(0), w.AnchorDate(0); a != d(6) || b != a {
		t.Fatalf("anchor full %s window %s, want %s", a, b, d(6))
	}
	if (&Result{In: &Input{Ledger: ledger}, Outcomes: []Outcome{{Ledger: []int32{0}}}}).AnchorDate(0) != 0 {
		t.Fatal("a hand-built result has no rules and no anchor")
	}
}
