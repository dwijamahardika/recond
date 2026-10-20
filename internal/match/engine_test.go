package match

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

var (
	day0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	wib  = time.FixedZone("UTC+7", 7*3600)
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{Providers: []ProviderRules{
		{Name: "psp_a", Format: "psp_a", Timezone: "+00:00", SettlementDays: 2, MatchWindow: Duration(10 * time.Minute),
			FeeToleranceMinor: 1, Fees: map[domain.Currency]domain.FeeRule{"IDR": {RateBPS: 150}}},
		{Name: "bank_b", Format: "bank_b", Timezone: "+07:00", SettlementDays: 1,
			Fees: map[domain.Currency]domain.FeeRule{"IDR": {RateBPS: 90, FixedMinor: 20_000}}},
	}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func led(id, provider, ref string, amount int64, at time.Time) domain.LedgerTxn {
	return domain.LedgerTxn{ID: id, Provider: provider, Reference: ref, Amount: amount, Currency: "IDR", CreatedAt: at}
}

// itemA builds a psp_a line with the expected fee unless fee >= 0 is given.
func itemA(id, ref string, gross int64, at time.Time, settled domain.Date, fee int64) domain.SettlementLine {
	if fee < 0 {
		fee = domain.FeeRule{RateBPS: 150}.Fee(gross)
	}
	return domain.SettlementLine{ID: "psp_a:" + id, Provider: "psp_a", Kind: domain.LineItem, Reference: ref, Currency: "IDR",
		Gross: gross, Fee: fee, Net: gross - fee, FeeKnown: true, TxnAt: at, SettledOn: settled}
}

func netB(gross int64) int64 {
	return gross - domain.FeeRule{RateBPS: 90, FixedMinor: 20_000}.Fee(gross)
}
func feeB(gross int64) int64 { return domain.FeeRule{RateBPS: 90, FixedMinor: 20_000}.Fee(gross) }

func payoutB(id string, from time.Time, count int, net, fee int64, settled domain.Date) domain.SettlementLine {
	return domain.SettlementLine{ID: "bank_b:" + id, Provider: "bank_b", Kind: domain.LinePayout, Reference: id, Currency: "IDR",
		Net: net, Fee: fee, Gross: net + fee, FeeKnown: true, SettledOn: settled,
		CoversFrom: from, CoversTo: from.Add(6 * time.Hour), ItemCount: count}
}

func d(day int) domain.Date { return domain.DateOf(day0, time.UTC).AddDays(day) }

type expect struct {
	class  domain.Class
	rule   domain.Rule
	ledger string // comma-separated ledger IDs
	lines  string // comma-separated line IDs
}

func run(t *testing.T, in *Input) *Result {
	t.Helper()
	if in.AsOf.IsZero() {
		in.AsOf = day0.AddDate(0, 0, 20)
	}
	res, err := Reconcile(testConfig(t), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func render(res *Result) []expect {
	out := make([]expect, 0, len(res.Outcomes))
	for _, o := range res.Outcomes {
		var l, s []string
		for _, i := range o.Ledger {
			l = append(l, res.In.Ledger[i].ID)
		}
		for _, i := range o.Lines {
			s = append(s, res.In.Lines[i].ID)
		}
		out = append(out, expect{o.Class, o.Rule, strings.Join(l, ","), strings.Join(s, ",")})
	}
	return out
}

func assertOutcomes(t *testing.T, res *Result, want ...expect) {
	t.Helper()
	got := render(res)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		var b strings.Builder
		for _, o := range res.Outcomes {
			fmt.Fprintf(&b, "  %+v\n", o)
		}
		t.Fatalf("outcomes\n got: %v\nwant: %v\n%s", got, want, b.String())
	}
}

func TestOneToOne(t *testing.T) {
	at := day0.Add(3 * time.Hour)
	cases := []struct {
		name   string
		ledger []domain.LedgerTxn
		lines  []domain.SettlementLine
		asOf   time.Time
		want   []expect
	}{
		{"exact reference",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at, d(2), -1)}, time.Time{},
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"reference normalised (case, whitespace)",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "  ord-1 ", 1_000_000, at, d(2), -1)}, time.Time{},
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"fee rounding within tolerance",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_100, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_100, at, d(2), 15_001)}, time.Time{},
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}}},
		{"amount mismatch",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 900_000, at, d(2), -1)}, time.Time{},
			[]expect{{"amount_mismatch", "reference", "t1", "psp_a:S1"}}},
		{"fee mismatch",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at, d(2), 25_000)}, time.Time{},
			[]expect{{"fee_mismatch", "reference", "t1", "psp_a:S1"}}},
		{"late settlement beyond T+2",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at, d(3), -1)}, time.Time{},
			[]expect{{"late_settlement", "reference", "t1", "psp_a:S1"}}},
		{"duplicate settlement",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "ORD-1", 1_000_000, at, d(2), -1), itemA("S9", "ORD-1", 1_000_000, at, d(2), -1)}, time.Time{},
			[]expect{{"matched", "reference", "t1", "psp_a:S1"}, {"duplicate_settlement", "reference", "t1", "psp_a:S9"}}},
		{"missing in settlement after deadline",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)}, nil, day0.AddDate(0, 0, 3),
			[]expect{{"missing_in_settlement", "unmatched", "t1", ""}}},
		{"awaiting settlement before deadline",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)}, nil, day0.AddDate(0, 0, 2).Add(23 * time.Hour),
			[]expect{{"awaiting_settlement", "unmatched", "t1", ""}}},
		{"missing in ledger",
			nil, []domain.SettlementLine{itemA("S1", "ORD-X", 1_000_000, at, d(2), -1)}, time.Time{},
			[]expect{{"missing_in_ledger", "unmatched", "", "psp_a:S1"}}},
		{"tolerance match on mangled reference",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at), led("t2", "psp_a", "ORD-2", 1_000_000, at.Add(time.Hour))},
			[]domain.SettlementLine{itemA("S1", "", 1_000_000, at.Add(2*time.Second), d(2), -1), itemA("S2", "REF/ORD-2", 1_000_000, at.Add(time.Hour-time.Second), d(2), -1)}, time.Time{},
			[]expect{{"matched", "tolerance", "t1", "psp_a:S1"}, {"matched", "tolerance", "t2", "psp_a:S2"}}},
		{"no tolerance match outside window",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{itemA("S1", "", 1_000_000, at.Add(11*time.Minute), d(2), -1)}, time.Time{},
			[]expect{{"missing_in_settlement", "unmatched", "t1", ""}, {"missing_in_ledger", "unmatched", "", "psp_a:S1"}}},
		{"currency mismatch",
			[]domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at)},
			[]domain.SettlementLine{func() domain.SettlementLine {
				l := itemA("S1", "ORD-1", 1_000_000, at, d(2), -1)
				l.Currency = "SGD"
				return l
			}()}, time.Time{},
			[]expect{{"amount_mismatch", "reference", "t1", "psp_a:S1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, &Input{AsOf: tc.asOf, Ledger: tc.ledger, Lines: tc.lines})
			assertOutcomes(t, res, tc.want...)
		})
	}
}

func TestRefundsDriveExpectedAmount(t *testing.T) {
	at := day0.Add(3 * time.Hour)
	pay := func(ref string, refunded int64, full bool) domain.Payment {
		return domain.Payment{Provider: "psp_a", Ref: ref, Lifecycle: domain.StatusPaid, Amount: 1_000_000, Currency: "IDR",
			RefundedAmount: refunded, FullyRefunded: full, PaidAt: at}
	}
	res := run(t, &Input{
		Ledger: []domain.LedgerTxn{led("t1", "psp_a", "ORD-1", 1_000_000, at), led("t2", "psp_a", "ORD-2", 1_000_000, at), led("t3", "psp_a", "ORD-3", 1_000_000, at)},
		Lines: []domain.SettlementLine{
			itemA("S1", "ORD-1", 600_000, at, d(2), -1),   // partial refund of 400k netted out
			itemA("S3", "ORD-3", 1_000_000, at, d(2), -1), // settled in full although partially refunded
		},
		Payments: map[domain.PaymentKey]domain.Payment{
			{Provider: "psp_a", Ref: "ORD-1"}: pay("ORD-1", 400_000, false),
			{Provider: "psp_a", Ref: "ORD-2"}: pay("ORD-2", 1_000_000, true),
			{Provider: "psp_a", Ref: "ORD-3"}: pay("ORD-3", 250_000, false),
		},
	})
	assertOutcomes(t, res,
		expect{"matched", "reference", "t1", "psp_a:S1"},
		expect{"amount_mismatch", "reference", "t3", "psp_a:S3"},
		expect{"matched", "refunded", "t2", ""},
	)
}

// The provider's paid_at (from webhooks) decides which payout window a
// transaction belongs to; ledger time is only the fallback.
func TestProviderTimeDecidesWindow(t *testing.T) {
	from := time.Date(2026, 10, 1, 6, 0, 0, 0, wib)
	paid := from.Add(-500 * time.Millisecond)   // provider: 05:59:59.5, previous window
	posted := from.Add(1500 * time.Millisecond) // ledger: 06:00:01.5, this window
	other := led("t2", "bank_b", "ORD-2", 2_000_000, from.Add(time.Hour))
	in := &Input{
		Ledger: []domain.LedgerTxn{led("t1", "bank_b", "ORD-1", 1_000_000, posted), other},
		Lines: []domain.SettlementLine{
			payoutB("PO-1", from.Add(-6*time.Hour), 1, netB(1_000_000), feeB(1_000_000), d(1)),
			payoutB("PO-2", from, 1, netB(2_000_000), feeB(2_000_000), d(1)),
		},
		Payments: map[domain.PaymentKey]domain.Payment{{Provider: "bank_b", Ref: "ORD-1"}: {Lifecycle: domain.StatusPaid, PaidAt: paid}},
	}
	assertOutcomes(t, run(t, in),
		expect{"matched", "batch", "t1", "bank_b:PO-1"},
		expect{"matched", "batch", "t2", "bank_b:PO-2"},
	)
}

func TestBatch(t *testing.T) {
	from := time.Date(2026, 10, 1, 6, 0, 0, 0, wib)
	amounts := []int64{1_000_000, 2_500_000, 750_000, 4_200_000, 300_000}
	ledger := make([]domain.LedgerTxn, 0, len(amounts))
	var net, fee int64
	for i, a := range amounts {
		ledger = append(ledger, led(fmt.Sprintf("t%d", i), "bank_b", fmt.Sprintf("ORD-%d", i), a, from.Add(time.Duration(i+1)*time.Minute)))
		net += netB(a)
		fee += feeB(a)
	}
	all := "t0,t1,t2,t3,t4"

	t.Run("whole window", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net, fee, d(1))}})
		assertOutcomes(t, res, expect{"matched", "batch", all, "bank_b:PO"})
	})
	t.Run("late payout", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net, fee, d(3))}})
		assertOutcomes(t, res, expect{"late_settlement", "batch", all, "bank_b:PO"})
	})
	t.Run("one transaction left out is identified", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 4, net-netB(750_000), fee-feeB(750_000), d(1))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", "t0,t1,t3,t4", "bank_b:PO"},
			expect{"missing_in_settlement", "unmatched", "t2", ""})
	})
	t.Run("two left out", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 3, net-netB(750_000)-netB(4_200_000), fee-feeB(750_000)-feeB(4_200_000), d(1))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", "t0,t1,t4", "bank_b:PO"},
			expect{"missing_in_settlement", "unmatched", "t2", ""},
			expect{"missing_in_settlement", "unmatched", "t3", ""})
	})
	t.Run("unknown count still finds a single exclusion", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 0, net-netB(300_000), fee-feeB(300_000), d(1))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", "t0,t1,t2,t3", "bank_b:PO"},
			expect{"missing_in_settlement", "unmatched", "t4", ""})
	})
	t.Run("tied amounts are not guessed", func(t *testing.T) {
		tied := append(append([]domain.LedgerTxn(nil), ledger...), led("t5", "bank_b", "ORD-5", 750_000, from.Add(10*time.Minute)))
		res := run(t, &Input{Ledger: tied, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net, fee, d(1))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", "t0,t1,t3,t4", "bank_b:PO"},
			expect{"missing_in_settlement", "batch", "t2,t5", ""})
	})
	t.Run("residual missing in ledger", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 6, net+netB(990_000), fee+feeB(990_000), d(1))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", all, "bank_b:PO"},
			expect{"missing_in_ledger", "batch_residual", "", "bank_b:PO"})
	})
	t.Run("fee mismatch when gross agrees", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net-50_000, fee+50_000, d(1))}})
		assertOutcomes(t, res, expect{"fee_mismatch", "batch", all, "bank_b:PO"})
	})
	t.Run("amount mismatch with same count", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net-123_400, fee, d(1))}})
		assertOutcomes(t, res, expect{"amount_mismatch", "batch", all, "bank_b:PO"})
	})
	t.Run("duplicate payout", func(t *testing.T) {
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{payoutB("PO", from, 5, net, fee, d(1)), payoutB("PO-DUP", from, 5, net, fee, d(2))}})
		assertOutcomes(t, res,
			expect{"matched", "batch", all, "bank_b:PO"},
			expect{"duplicate_settlement", "batch", "", "bank_b:PO-DUP"})
	})
	t.Run("payout with nothing in ledger", func(t *testing.T) {
		res := run(t, &Input{Lines: []domain.SettlementLine{payoutB("PO", from, 5, net, fee, d(1))}})
		assertOutcomes(t, res, expect{"missing_in_ledger", "batch", "", "bank_b:PO"})
	})
	t.Run("itemised late credit leaves the batch clean", func(t *testing.T) {
		late := domain.SettlementLine{ID: "bank_b:late", Provider: "bank_b", Kind: domain.LineItem, Reference: "ORD-2", Currency: "IDR",
			Gross: 750_000, Fee: feeB(750_000), Net: netB(750_000), FeeKnown: true, TxnAt: ledger[2].CreatedAt, SettledOn: d(6)}
		res := run(t, &Input{Ledger: ledger, Lines: []domain.SettlementLine{
			payoutB("PO", from, 4, net-netB(750_000), fee-feeB(750_000), d(1)), late}})
		assertOutcomes(t, res, // the reference pass runs first
			expect{"late_settlement", "reference", "t2", "bank_b:late"},
			expect{"matched", "batch", "t0,t1,t3,t4", "bank_b:PO"})
	})
}

func TestSubsetSearch(t *testing.T) {
	e := &engine{}
	vals := []int64{5, 9, 13, 21, 34, 55, 89, 144}
	cands := make([]int32, len(vals))
	e.txn = make([]txnState, len(vals))
	for i, v := range vals {
		cands[i] = int32(i)
		e.txn[i].exp.Net = v
	}
	cases := []struct {
		m      int
		target int64
		sols   int
	}{
		{1, 21, 1}, {1, 22, 0},
		{2, 34, 1},  // 13+21
		{2, 144, 1}, // 55+89
		{3, 27, 1},  // 5+9+13
		{3, 1000, 0},
		{4, 48, 1}, // 5+9+13+21
	}
	for _, tc := range cases {
		res := searchExclusions(e, cands, tc.m, tc.target, 0, 1_000_000)
		got := len(res.solutions)
		if got != tc.sols || !res.complete {
			t.Errorf("m=%d target=%d: %d solutions (complete=%v), want %d: %v", tc.m, tc.target, got, res.complete, tc.sols, res.solutions)
		}
		for _, s := range res.solutions {
			var sum int64
			for _, i := range s {
				sum += vals[i]
			}
			if sum != tc.target || len(s) != tc.m {
				t.Errorf("bad solution %v", s)
			}
		}
	}
	// A tiny budget must report incompleteness instead of a wrong answer.
	if res := searchExclusions(e, cands, 3, 27, 0, 2); res.complete {
		t.Error("expected incomplete search with exhausted budget")
	}
}

// Same inputs in any order must give the same outcomes (by ID) and digest.
func TestDeterministicAcrossInputOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	at := day0.Add(time.Hour)
	var ledger []domain.LedgerTxn
	var lines []domain.SettlementLine
	for i := 0; i < 300; i++ {
		ref := fmt.Sprintf("ORD-%d", i)
		amt := int64(10_000 + rng.Intn(50)*1000) // many ties on purpose
		ts := at.Add(time.Duration(rng.Intn(3600)) * time.Second)
		ledger = append(ledger, led(fmt.Sprintf("t%03d", i), "psp_a", ref, amt, ts))
		switch rng.Intn(5) {
		case 0: // drop
		case 1:
			lines = append(lines, itemA(fmt.Sprintf("S%03d", i), "", amt, ts, d(2), -1))
		default:
			lines = append(lines, itemA(fmt.Sprintf("S%03d", i), ref, amt, ts, d(2), -1))
		}
	}
	base := run(t, &Input{Ledger: ledger, Lines: lines}).Digest()
	for k := 0; k < 5; k++ {
		l2 := append([]domain.LedgerTxn(nil), ledger...)
		s2 := append([]domain.SettlementLine(nil), lines...)
		rng.Shuffle(len(l2), func(i, j int) { l2[i], l2[j] = l2[j], l2[i] })
		rng.Shuffle(len(s2), func(i, j int) { s2[i], s2[j] = s2[j], s2[i] })
		if got := run(t, &Input{Ledger: l2, Lines: s2}).Digest(); got != base {
			t.Fatalf("digest changed with input order: %s vs %s", got, base)
		}
	}
}

func TestUnknownProviderIsConfigError(t *testing.T) {
	_, err := Reconcile(testConfig(t), &Input{Ledger: []domain.LedgerTxn{led("t1", "psp_z", "R", 1, day0)}})
	if err == nil || !strings.Contains(err.Error(), "psp_z") {
		t.Fatalf("want config error naming psp_z, got %v", err)
	}
}
