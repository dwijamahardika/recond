package gen

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/ingest"
)

func (g *generator) pick(kinds []string) string {
	var on []string
	for _, k := range kinds {
		if g.cfg.Faults[k] {
			on = append(on, k)
		}
	}
	if len(on) == 0 {
		return ""
	}
	return on[g.rng.Intn(len(on))]
}

func (g *generator) sid(prefix string) string {
	g.nextSID++
	return fmt.Sprintf("STL-A-%s%08d", prefix, g.nextSID)
}

// settlePSPA writes psp_a's itemised settlement (T+N per transaction) and
// injects item faults.
func (g *generator) settlePSPA() {
	r := g.ra
	loc := r.Location()
	for _, p := range g.pays {
		if p.provider != ProviderA || p.ledger == nil || p.exp.Gross == 0 {
			continue // expired, or fully refunded before settlement: nothing settles
		}
		due := domain.DateOf(p.paidAt, loc).AddDays(r.SettlementDays)
		row := SettlementRow{SettlementID: g.sid("0"), Ref: p.ref, TxnTime: p.paidAt, Date: due, Gross: p.exp.Gross, Fee: p.exp.Fee}
		// Clean rows: the provider rounds differently now and then, within the
		// configured tolerance. These must never be flagged.
		if tol := r.FeeToleranceMinor; tol > 0 && g.rng.Intn(5) == 0 {
			if j := 1 - 2*int64(g.rng.Intn(2)); row.Fee+j >= 0 {
				row.Fee += j
			}
		}
		id := ProviderA + ":" + row.SettlementID
		lid := p.ledger.ID
		kind := ""
		if g.rng.Float64() < g.cfg.FaultRate {
			kind = g.pick(ItemFaults)
		}
		switch kind {
		case FaultDropSettlement:
			if !g.openTxn(p.exp.Gross, p.paidAt) {
				break
			}
			g.fault(Fault{Kind: kind, Class: domain.ClassMissingInSettlement, Provider: ProviderA, Key: lid, LedgerID: lid})
			continue
		case FaultDropLedger:
			if !g.openLine(p.exp.Gross, p.paidAt) {
				break
			}
			p.ledger = nil
			g.fault(Fault{Kind: kind, Class: domain.ClassMissingInLedger, Provider: ProviderA, Key: id, LineID: id, LedgerID: lid})
		case FaultDuplicate:
			dup := row
			dup.SettlementID = g.sid("D") // sorts after the original on the same date
			dup.Date = due.AddDays(g.rng.Intn(2))
			dupID := ProviderA + ":" + dup.SettlementID
			g.ds.PSPA[dup.Date] = append(g.ds.PSPA[dup.Date], PSPAItem{ID: dupID, SettlementRow: dup})
			g.fault(Fault{Kind: kind, Class: domain.ClassDuplicateSettlement, Provider: ProviderA, Key: dupID, LineID: dupID, LedgerID: lid})
		case FaultAmountChange:
			delta := (1_000 + g.rng.Int63n(max(1, row.Gross/100/10))) * 100
			if g.rng.Intn(2) == 0 && row.Gross > delta {
				delta = -delta
			}
			row.Gross += delta
			row.Fee = r.Fees[Currency].Fee(row.Gross)
			g.fault(Fault{Kind: kind, Class: domain.ClassAmountMismatch, Provider: ProviderA, Key: id, LineID: id, LedgerID: lid,
				Detail: fmt.Sprintf("gross %+d", delta)})
		case FaultFeeChange:
			delta := (100 + g.rng.Int63n(5_000)) * 100
			if g.rng.Intn(3) == 0 && row.Fee > delta {
				delta = -delta
			}
			if delta >= row.Gross-row.Fee {
				break
			}
			row.Fee = p.exp.Fee + delta
			g.fault(Fault{Kind: kind, Class: domain.ClassFeeMismatch, Provider: ProviderA, Key: id, LineID: id, LedgerID: lid,
				Detail: fmt.Sprintf("fee %+d", delta)})
		case FaultDelay:
			row.Date = due.AddDays(1 + g.rng.Intn(5))
			g.fault(Fault{Kind: kind, Class: domain.ClassLateSettlement, Provider: ProviderA, Key: id, LineID: id, LedgerID: lid,
				Detail: fmt.Sprintf("settled %s, due %s", row.Date, due)})
		case FaultCorruptReference:
			mode := g.rng.Intn(4)
			if mode != 3 && !g.openPair(p.exp.Gross, p.paidAt) {
				break
			}
			switch mode {
			case 0:
				row.Ref = ""
			case 1:
				row.Ref = row.Ref[:len(row.Ref)-2]
			case 2:
				row.Ref = "REF/" + row.Ref
			case 3: // healed by normalisation, matched by reference
				row.Ref = "  " + strings.ToLower(row.Ref) + " "
			}
			g.fault(Fault{Kind: kind, Class: domain.ClassMatched, Provider: ProviderA, Key: id, LineID: id, LedgerID: lid,
				Detail: fmt.Sprintf("reference sent as %q", row.Ref)})
		}
		g.ds.PSPA[row.Date] = append(g.ds.PSPA[row.Date], PSPAItem{ID: id, SettlementRow: row})
		if p.ledger != nil {
			g.ds.Truth.Settles[lid] = id
		}
		g.settledEvent(p, row.Date)
	}
}

// ---- bank_b ----

type cycle struct {
	from    time.Time
	members []*payment
}

// settleBankB groups bank_b transactions into payout cycles (provider clock),
// writes one unitemised payout per cycle and injects batch faults.
func (g *generator) settleBankB() {
	r := g.rb
	loc := r.Location()
	step := time.Duration(g.cfg.CycleHours) * time.Hour
	cycles := map[int64]*cycle{}
	for _, p := range g.pays {
		if p.provider != ProviderB || p.ledger == nil || p.exp.Gross == 0 {
			continue
		}
		local := p.paidAt.In(loc)
		y, m, d := local.Date()
		dayStart := time.Date(y, m, d, 0, 0, 0, 0, loc)
		from := dayStart.Add(local.Sub(dayStart) / step * step)
		c := cycles[from.Unix()]
		if c == nil {
			c = &cycle{from: from}
			cycles[from.Unix()] = c
		}
		c.members = append(c.members, p)
	}
	keys := make([]int64, 0, len(cycles))
	for k := range cycles {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, k := range keys {
		c := cycles[k]
		members := c.members
		valueDate := domain.DateOf(c.from, loc).AddDays(r.SettlementDays)
		var credit, charges int64
		for _, p := range members {
			credit += p.exp.Net
			charges += p.exp.Fee
		}
		po := &BankBEntry{ID: g.token(), ValueDate: valueDate,
			RefNo: fmt.Sprintf("PO-B-%s-%02d", c.from.In(loc).Format("20060102"), c.from.In(loc).Hour())}
		settled := members
		kind := ""
		if g.rng.Float64() < g.cfg.BatchFaultRate {
			kind = g.pick(BatchFaults)
		}
		switch kind {
		case FaultBatchDrop:
			x := g.uniqueNetMember(members)
			if x == nil {
				break
			}
			settled = remove(members, x)
			credit -= x.exp.Net
			charges -= x.exp.Fee
			g.fault(Fault{Kind: kind, Class: domain.ClassMissingInSettlement, Provider: ProviderB, Key: x.ledger.ID, LedgerID: x.ledger.ID,
				Detail: "left out of " + po.RefNo})
		case FaultBatchLedgerDrop:
			x := members[g.rng.Intn(len(members))]
			g.fault(Fault{Kind: kind, Class: domain.ClassMissingInLedger, Provider: ProviderB, Key: po.ID, LineID: po.ID, LedgerID: x.ledger.ID,
				Detail: "ledger lacks " + x.ref})
			x.ledger = nil
		case FaultBatchAmount:
			delta := (1_000 + g.rng.Int63n(50_000)) * 100
			if g.rng.Intn(2) == 0 && credit > 2*delta {
				delta = -delta
			}
			credit += delta
			g.fault(Fault{Kind: kind, Class: domain.ClassAmountMismatch, Provider: ProviderB, Key: po.ID, LineID: po.ID, Detail: fmt.Sprintf("credit %+d", delta)})
		case FaultBatchFee:
			delta := (500 + g.rng.Int63n(20_000)) * 100
			if credit <= 2*delta {
				break
			}
			credit -= delta
			charges += delta
			g.fault(Fault{Kind: kind, Class: domain.ClassFeeMismatch, Provider: ProviderB, Key: po.ID, LineID: po.ID, Detail: fmt.Sprintf("charges +%d", delta)})
		case FaultBatchDelay:
			po.ValueDate = valueDate.AddDays(1 + g.rng.Intn(4))
			g.fault(Fault{Kind: kind, Class: domain.ClassLateSettlement, Provider: ProviderB, Key: po.ID, LineID: po.ID,
				Detail: fmt.Sprintf("paid %s, due %s", po.ValueDate, valueDate)})
		case FaultItemDelay:
			if len(members) < 2 {
				break
			}
			x := members[g.rng.Intn(len(members))]
			settled = remove(members, x)
			credit -= x.exp.Net
			charges -= x.exp.Fee
			due := domain.DateOf(x.paidAt, loc).AddDays(r.SettlementDays)
			late := BankBEntry{ID: g.token(), ValueDate: due.AddDays(1 + g.rng.Intn(5)), TxnTime: x.paidAt, RefNo: x.ref,
				Description: "QR CREDIT", Credit: x.exp.Net, Charges: x.exp.Fee}
			g.ds.BankB[late.ValueDate] = append(g.ds.BankB[late.ValueDate], late)
			g.ds.Truth.Settles[x.ledger.ID] = late.ID
			g.settledEvent(x, late.ValueDate)
			g.fault(Fault{Kind: kind, Class: domain.ClassLateSettlement, Provider: ProviderB, Key: late.ID, LineID: late.ID, LedgerID: x.ledger.ID,
				Detail: fmt.Sprintf("credited alone %s, due %s", late.ValueDate, due)})
		}
		po.Credit, po.Charges = credit, charges
		po.Description = ingest.PayoutDescription(len(settled), c.from, c.from.Add(step), loc)
		g.ds.BankB[po.ValueDate] = append(g.ds.BankB[po.ValueDate], *po)
		if kind == FaultBatchDuplicate {
			dup := *po
			dup.ID, dup.RefNo, dup.ValueDate = g.token(), po.RefNo+"R", po.ValueDate.AddDays(1)
			g.ds.BankB[dup.ValueDate] = append(g.ds.BankB[dup.ValueDate], dup)
			g.fault(Fault{Kind: kind, Class: domain.ClassDuplicateSettlement, Provider: ProviderB, Key: dup.ID, LineID: dup.ID,
				Detail: "repeats " + po.RefNo})
		}
		for _, p := range settled {
			if p.ledger != nil {
				g.ds.Truth.Settles[p.ledger.ID] = po.ID
			}
			g.settledEvent(p, po.ValueDate)
		}
	}
	g.assignBankBIDs()
}

// uniqueNetMember picks a member whose expected net no other member shares:
// only then can a reconciler tell which transaction a short payout omitted.
func (g *generator) uniqueNetMember(members []*payment) *payment {
	if len(members) < 2 {
		return nil
	}
	count := map[int64]int{}
	for _, p := range members {
		count[p.exp.Net]++
	}
	start := g.rng.Intn(len(members))
	for i := range members {
		p := members[(start+i)%len(members)]
		if count[p.exp.Net] == 1 {
			return p
		}
	}
	return nil
}

func remove(ps []*payment, x *payment) []*payment {
	out := make([]*payment, 0, len(ps)-1)
	for _, p := range ps {
		if p != x {
			out = append(out, p)
		}
	}
	return out
}

// token is a placeholder line ID; bank_b IDs depend on the rendered row and
// its position in the statement, so they are assigned once all rows exist.
func (g *generator) token() string {
	g.nextSID++
	return fmt.Sprintf("\x00tok%d", g.nextSID)
}

func (g *generator) assignBankBIDs() {
	assigned := map[string]string{}
	for date, entries := range g.ds.BankB {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].RefNo < entries[j].RefNo })
		w, _ := ingest.NewBankBWriter(io.Discard, ProviderB, g.rb.Location())
		for i := range entries {
			id, _ := w.Write(entries[i].row())
			assigned[entries[i].ID] = id
			entries[i].ID = id
		}
		g.ds.BankB[date] = entries
	}
	fix := func(s string) string {
		if v, ok := assigned[s]; ok {
			return v
		}
		return s
	}
	for i := range g.ds.Truth.Faults {
		f := &g.ds.Truth.Faults[i]
		f.Key, f.LineID = fix(f.Key), fix(f.LineID)
	}
	for k, v := range g.ds.Truth.Settles {
		g.ds.Truth.Settles[k] = fix(v)
	}
}

func (e BankBEntry) row() ingest.BankBRow {
	return ingest.BankBRow{ValueDate: e.ValueDate, TxnTime: e.TxnTime, RefNo: e.RefNo, Description: e.Description,
		Credit: e.Credit, Charges: e.Charges, ChargesKnown: true, Currency: Currency}
}

// ---- identifiability guard for the tolerance pass ----
//
// After the reference pass, psp_a lines whose reference matches nothing
// ("open lines": dropped from the ledger, or reference corrupted) compete for
// psp_a transactions with no settlement ("open transactions": dropped from
// settlement, or whose line is corrupted) on amount and time. A fault that
// would give an open line a second plausible partner is not injected.

func (g *generator) window() time.Duration { return 2 * time.Duration(g.ra.MatchWindow) }

func near(ts []time.Time, t time.Time, w time.Duration) bool {
	for _, x := range ts {
		if d := x.Sub(t); d <= w && d >= -w {
			return true
		}
	}
	return false
}

func (g *generator) openTxn(gross int64, at time.Time) bool {
	if near(g.openLines[gross], at, g.window()) {
		return false
	}
	g.openTxns[gross] = append(g.openTxns[gross], at)
	return true
}

func (g *generator) openLine(gross int64, at time.Time) bool {
	if near(g.openTxns[gross], at, g.window()) {
		return false
	}
	g.openLines[gross] = append(g.openLines[gross], at)
	return true
}

func (g *generator) openPair(gross int64, at time.Time) bool {
	if near(g.openTxns[gross], at, g.window()) || near(g.openLines[gross], at, g.window()) {
		return false
	}
	g.openTxns[gross] = append(g.openTxns[gross], at)
	g.openLines[gross] = append(g.openLines[gross], at)
	return true
}
