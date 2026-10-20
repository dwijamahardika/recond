package match

import (
	"fmt"
	"sort"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// N:1 payouts.
//
// A payout is one credit covering many transactions minus fees, and the bank
// does not itemise it. What it does state is the window it covers (provider
// clock) and, usually, how many transactions are in it. The candidates are the
// transactions still unmatched after the 1:1 passes whose provider time falls
// in that window; each has an expected net (gross - refunds - fee).
//
//   - Sum of candidate nets == payout (and counts agree): the whole window
//     settled. This is the overwhelmingly common case and costs O(n).
//   - Payout short and the count says m transactions are missing: find which
//     m candidates to exclude so the rest sums to the payout. This is
//     subset-sum - NP-hard in general - but m is small in practice (the
//     search is capped at MaxBatchExclusions) and searching for the m-item
//     *complement* is O(n) for m=1, O(n log n) for m=2 and a pruned DFS with
//     a node budget beyond that.
//   - Several exclusions explain the payout equally well (equal amounts):
//     recond refuses to guess. Transactions in every solution stay matched;
//     the tied ones are reported together as missing_in_settlement for review.
//   - Payout larger and the count says k more items than the ledger has: the
//     candidates are matched and the residual is missing_in_ledger.
//   - Otherwise the batch is a mismatch, localised to fee_mismatch when
//     gross agrees and only the charges differ.
//
// Limits: one payout per provider window (a window split across several
// payouts would need a partition search); amounts within one exclusion search
// must match exactly up to batch_tolerance_minor; with no item count, only up
// to MaxBatchExclusions exclusions are tried and a payout that is too large is
// an amount_mismatch because the number of unknown items is unknowable.

type payoutKey struct {
	provider string
	currency domain.Currency
	from, to int64
	net      int64
	count    int
}

func (e *engine) batches() {
	type poolEntry = int32
	pools := map[poolKey][]poolEntry{}
	for _, i := range e.ledOrder { // already in provider-time order within a provider
		if s := &e.txn[i]; !s.matched && s.rules != nil && s.exp.Gross > 0 {
			k := poolKey{e.in.Ledger[i].Provider, e.in.Ledger[i].Currency}
			pools[k] = append(pools[k], i)
		}
	}
	seen := map[payoutKey]int32{}
	for _, li := range e.lineOrder {
		p := &e.in.Lines[li]
		if p.Kind != domain.LinePayout || e.used[li] {
			continue
		}
		e.used[li] = true
		r, _ := e.cfg.Provider(p.Provider)
		pool := pools[poolKey{p.Provider, p.Currency}]
		lo := sort.Search(len(pool), func(k int) bool { return !e.txn[pool[k]].provTime.Before(p.CoversFrom) })
		var cands []int32
		for k := lo; k < len(pool) && e.txn[pool[k]].provTime.Before(p.CoversTo); k++ {
			if !e.txn[pool[k]].matched {
				cands = append(cands, pool[k])
			}
		}
		key := payoutKey{p.Provider, p.Currency, p.CoversFrom.UnixNano(), p.CoversTo.UnixNano(), p.Net, p.ItemCount}
		deadline := domain.DateOf(p.CoversFrom, r.Location()).AddDays(r.SettlementDays)
		if len(cands) == 0 {
			if orig, ok := seen[key]; ok {
				e.emit(Outcome{
					Class: domain.ClassDuplicateSettlement, Rule: domain.RuleBatch, Provider: p.Provider, Currency: p.Currency,
					Lines: []int32{li}, Context: []int32{orig}, Actual: Amounts{p.Gross, p.Fee, p.Net},
					Deadline: deadline, SettledOn: p.SettledOn,
					Detail: fmt.Sprintf("payout repeats %s (same window, count and amount)", e.in.Lines[orig].Reference),
				})
				continue
			}
			e.emit(Outcome{
				Class: domain.ClassMissingInLedger, Rule: domain.RuleBatch, Provider: p.Provider, Currency: p.Currency,
				Lines: []int32{li}, Actual: Amounts{p.Gross, p.Fee, p.Net}, Deadline: deadline, SettledOn: p.SettledOn,
				Detail: fmt.Sprintf("payout covers %d transaction(s) in %s; none unmatched in ledger", p.ItemCount, window(p, r.Location())),
			})
			continue
		}
		if _, ok := seen[key]; !ok {
			seen[key] = li
		}
		e.settleBatch(li, cands, r, deadline)
	}
}

type sums struct {
	Amounts
	overflow bool
}

func (e *engine) sum(members []int32) sums {
	var s sums
	for _, i := range members {
		x := e.txn[i].exp
		var ok1, ok2, ok3 bool
		s.Gross, ok1 = domain.AddChecked(s.Gross, x.Gross)
		s.Fee, ok2 = domain.AddChecked(s.Fee, x.Fee)
		s.Net, ok3 = domain.AddChecked(s.Net, x.Net)
		s.overflow = s.overflow || !ok1 || !ok2 || !ok3
	}
	return s
}

func (e *engine) settleBatch(li int32, cands []int32, r *ProviderRules, deadline domain.Date) {
	p := &e.in.Lines[li]
	n, k := len(cands), p.ItemCount
	total := e.sum(cands)
	if total.overflow {
		e.batchMismatch(li, cands, deadline, total, "candidate total overflows")
		return
	}
	diff, tol := total.Net-p.Net, r.BatchToleranceMinor

	switch {
	case abs(diff) <= tol && (k == 0 || k == n):
		e.batchGroup(li, cands, deadline, "")
		return

	case diff > 0 && (k == 0 || k < n):
		var tries []int
		if k > 0 {
			tries = []int{n - k}
		} else {
			for m := 1; m <= r.MaxBatchExclusions; m++ {
				tries = append(tries, m)
			}
		}
		for _, m := range tries {
			if m <= 0 || m >= n || m > r.MaxBatchExclusions {
				continue
			}
			res := searchExclusions(e, cands, m, diff, tol, e.cfg.BatchSearchBudget)
			switch {
			case len(res.solutions) == 1:
				members := without(cands, res.solutions[0])
				e.batchGroup(li, members, deadline, fmt.Sprintf("%d of %d transactions in window; %d excluded by subset search", len(members), n, m))
				return
			case len(res.solutions) > 1 && res.complete:
				amb := res.union(cands)
				members := without(cands, amb)
				e.batchGroup(li, members, deadline, fmt.Sprintf("%d of %d transactions in window; %d of %d tied candidates excluded", len(members), n, m, len(amb)))
				for _, i := range amb {
					e.txn[i].matched = true
				}
				s := e.sum(amb)
				e.emit(Outcome{
					Class: domain.ClassMissingInSettlement, Rule: domain.RuleBatch, Provider: p.Provider, Currency: p.Currency,
					Ledger: amb, Context: []int32{li}, Expected: s.Amounts, Deadline: deadline,
					Detail: fmt.Sprintf("payout %s is short by %s: %d of these %d transactions are not in it, but their amounts tie so recond cannot tell which",
						p.Reference, money(diff, p.Currency), m, len(amb)),
				})
				return
			case len(res.solutions) > 1:
				e.batchMismatch(li, cands, deadline, total, fmt.Sprintf("shortfall %s is explained by many %d-transaction subsets (search incomplete)", money(diff, p.Currency), m))
				return
			}
		}
		e.batchMismatch(li, cands, deadline, total, fmt.Sprintf("no subset of up to %d transactions explains the shortfall of %s", r.MaxBatchExclusions, money(diff, p.Currency)))
		return

	case diff < 0 && k > n:
		e.batchGroup(li, cands, deadline, fmt.Sprintf("payout states %d transactions, ledger has %d in window", k, n))
		e.emit(Outcome{
			Class: domain.ClassMissingInLedger, Rule: domain.RuleBatchResidual, Provider: p.Provider, Currency: p.Currency,
			Lines: []int32{li}, Actual: Amounts{Net: -diff}, Deadline: deadline, SettledOn: p.SettledOn,
			Detail: fmt.Sprintf("%d transaction(s) worth %s net in payout %s have no ledger entry", k-n, money(-diff, p.Currency), p.Reference),
		})
		return
	}
	e.batchMismatch(li, cands, deadline, total, "")
}

// batchGroup records members as settled by payout li.
func (e *engine) batchGroup(li int32, members []int32, deadline domain.Date, note string) {
	p := &e.in.Lines[li]
	r, _ := e.cfg.Provider(p.Provider)
	for _, i := range members {
		e.txn[i].matched = true
	}
	o := Outcome{
		Class: domain.ClassMatched, Rule: domain.RuleBatch, Provider: p.Provider, Currency: p.Currency,
		Ledger: members, Lines: []int32{li}, Expected: e.sum(members).Amounts, Actual: Amounts{p.Gross, p.Fee, p.Net},
		Deadline: deadline, SettledOn: p.SettledOn, Detail: note,
	}
	if p.SettledOn > deadline {
		o.Class = domain.ClassLateSettlement
		o.Detail = joinNote(note, fmt.Sprintf("payout settled %s, due by %s (T+%d)", p.SettledOn, deadline, r.SettlementDays))
	}
	e.emit(o)
}

// batchMismatch flags the whole window: money moved, but the payout cannot be
// reconciled to individual transactions.
func (e *engine) batchMismatch(li int32, cands []int32, deadline domain.Date, total sums, why string) {
	p := &e.in.Lines[li]
	r, _ := e.cfg.Provider(p.Provider)
	for _, i := range cands {
		e.txn[i].matched = true
	}
	o := Outcome{
		Class: domain.ClassAmountMismatch, Rule: domain.RuleBatch, Provider: p.Provider, Currency: p.Currency,
		Ledger: cands, Lines: []int32{li}, Expected: total.Amounts, Actual: Amounts{p.Gross, p.Fee, p.Net},
		Deadline: deadline, SettledOn: p.SettledOn,
	}
	countsAgree := p.ItemCount == 0 || p.ItemCount == len(cands)
	if p.FeeKnown && countsAgree && !total.overflow && abs(p.Gross-total.Gross) <= r.BatchToleranceMinor {
		o.Class = domain.ClassFeeMismatch
		o.Detail = joinNote(fmt.Sprintf("gross agrees; charges %s, expected %s", money(p.Fee, p.Currency), money(total.Fee, p.Currency)), why)
	} else {
		o.Detail = joinNote(fmt.Sprintf("payout net %s for %d transaction(s); ledger expects %s for %d", money(p.Net, p.Currency), p.ItemCount, money(total.Net, p.Currency), len(cands)), why)
	}
	e.emit(o)
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func window(p *domain.SettlementLine, loc *time.Location) string {
	return p.CoversFrom.In(loc).Format("2006-01-02 15:04") + " to " + p.CoversTo.In(loc).Format("2006-01-02 15:04")
}

// without returns cands minus drop, preserving order.
func without(cands, drop []int32) []int32 {
	skip := make(map[int32]bool, len(drop))
	for _, d := range drop {
		skip[d] = true
	}
	out := make([]int32, 0, len(cands)-len(drop))
	for _, c := range cands {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out
}

// ---- subset search ----

type exclusionResult struct {
	solutions [][]int32 // each: ledger indices to exclude
	complete  bool      // all solutions were enumerated
}

// union returns every candidate that appears in some solution, in cands
// order (which is provider-time order, independent of input order).
func (r exclusionResult) union(cands []int32) []int32 {
	set := map[int32]bool{}
	for _, s := range r.solutions {
		for _, i := range s {
			set[i] = true
		}
	}
	out := make([]int32, 0, len(set))
	for _, c := range cands {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

const maxSolutions = 64

// searchExclusions finds m candidates whose expected nets sum to target
// (within tol). For m=1 every solution is enumerated directly. For m>=2 a DFS
// over values sorted ascending prunes on the smallest and largest sums still
// reachable, finishes each branch with a binary search, and stops after
// maxSolutions solutions or budget nodes - in which case complete is false
// and the caller treats the batch as unresolved rather than guessing.
func searchExclusions(e *engine, cands []int32, m int, target, tol int64, budget int) exclusionResult {
	vals := make([]int64, len(cands))
	order := make([]int32, len(cands))
	for i := range cands {
		order[i] = int32(i) //nolint:gosec // a window is a subset of the int32-bounded input
	}
	sort.Slice(order, func(a, b int) bool {
		va, vb := e.txn[cands[order[a]]].exp.Net, e.txn[cands[order[b]]].exp.Net
		if va != vb {
			return va < vb
		}
		return order[a] < order[b]
	})
	for i, o := range order {
		vals[i] = e.txn[cands[o]].exp.Net
	}
	s := &subsetSearch{vals: vals, tol: tol, budget: budget}
	if m == 1 {
		s.limit = len(vals) + 1 // enumerate all singletons
	} else {
		s.limit = maxSolutions
	}
	s.dfs(0, m, target)
	res := exclusionResult{complete: !s.exhausted && len(s.sols) < s.limit}
	for _, sol := range s.sols {
		idx := make([]int32, len(sol))
		for j, pos := range sol {
			idx[j] = cands[order[pos]]
		}
		res.solutions = append(res.solutions, idx)
	}
	return res
}

type subsetSearch struct {
	vals      []int64 // ascending
	tol       int64
	limit     int
	budget    int
	nodes     int
	exhausted bool
	stack     []int
	sols      [][]int
}

func (s *subsetSearch) dfs(start, k int, target int64) {
	if len(s.sols) >= s.limit || s.exhausted {
		return
	}
	if s.nodes++; s.nodes > s.budget {
		s.exhausted = true
		return
	}
	n := len(s.vals)
	if k == 1 {
		lo := start + sort.Search(n-start, func(i int) bool { return s.vals[start+i] >= target-s.tol })
		for j := lo; j < n && s.vals[j] <= target+s.tol; j++ {
			sol := append(append([]int(nil), s.stack...), j)
			s.sols = append(s.sols, sol)
			if len(s.sols) >= s.limit {
				return
			}
		}
		return
	}
	// The k-1 largest values bound what the rest of any branch can add.
	// k is small (MaxBatchExclusions), so sums are taken directly rather than
	// from prefix sums that could overflow on adversarial input.
	maxRest := s.sum(n-k+1, n)
	for i := start; i <= n-k; i++ {
		// Smallest sum with vals[i] first: it and the next k-1. Sorted
		// ascending, so once this overshoots every later i does too.
		if s.sum(i, i+k) > target+s.tol {
			break
		}
		if satAdd(s.vals[i], maxRest) < target-s.tol {
			continue
		}
		s.stack = append(s.stack, i)
		s.dfs(i+1, k-1, target-s.vals[i])
		s.stack = s.stack[:len(s.stack)-1]
		if len(s.sols) >= s.limit || s.exhausted {
			return
		}
	}
}

func (s *subsetSearch) sum(from, to int) int64 {
	var t int64
	for _, v := range s.vals[from:to] {
		t = satAdd(t, v)
	}
	return t
}

func satAdd(a, b int64) int64 {
	if v, ok := domain.AddChecked(a, b); ok {
		return v
	}
	if a > 0 {
		return 1<<63 - 1
	}
	return -1 << 63
}
