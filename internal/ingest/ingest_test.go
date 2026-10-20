package ingest

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

var wib = time.FixedZone("UTC+7", 7*3600)

func readLines(t *testing.T, a Adapter, src string) ([]domain.SettlementLine, []Reject, error) {
	t.Helper()
	var lines []domain.SettlementLine
	var rejects []Reject
	err := a.Read(strings.NewReader(src), Options{Provider: a.Format(), Location: wib}, func(l domain.SettlementLine, _ string) error {
		lines = append(lines, l)
		return nil
	}, func(r Reject) { rejects = append(rejects, r) })
	return lines, rejects, err
}

func TestPSPA(t *testing.T) {
	src := "\ufeffsettlement_id,merchant_ref,transaction_time,settlement_date,currency,gross_amount,fee_amount,net_amount,extra_column\n" +
		"STL-1,ORD-1,2026-10-01T03:15:22Z,2026-10-03,IDR,150000.00,2250.00,147750.00,ignored\n" +
		"STL-2,,2026-10-01T03:15:22+07:00,2026-10-03,idr,1000,15,985,\n" +
		"STL-3,ORD-3,2026-10-01T03:15:22Z,2026-10-03,IDR,1,500.00,0.15,1499.85\n" + // wrong column count shifts fields
		"STL-4,ORD-4,2026-10-01T03:15:22Z,2026-10-03,IDR,100.00,1.00,98.00\n" + // net inconsistent
		"STL-5,ORD-5,2026-10-01T03:15:22Z,2026-10-03,IDR,1.500.000,00,0.00,0.00\n" + // bank_b-style number in psp_a file
		"STL-6,ORD-6,yesterday,2026-10-03,IDR,1.00,0.00,1.00\n" +
		"STL-7,ORD-7,2026-10-01T03:15:22Z,2026-10-03,XXX,1.00,0.00,1.00\n"
	lines, rejects, err := readLines(t, PSPA{}, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || len(rejects) != 5 {
		t.Fatalf("lines=%d rejects=%d %+v", len(lines), len(rejects), rejects)
	}
	l := lines[0]
	if l.ID != "psp_a:STL-1" || l.Reference != "ORD-1" || l.Gross != 15_000_000 || l.Fee != 225_000 || l.Net != 14_775_000 || !l.FeeKnown {
		t.Fatalf("bad line %+v", l)
	}
	if l.SettledOn.String() != "2026-10-03" || !l.TxnAt.Equal(time.Date(2026, 10, 1, 3, 15, 22, 0, time.UTC)) {
		t.Fatalf("bad dates %+v", l)
	}
	if lines[1].Currency != "IDR" || !lines[1].TxnAt.Equal(time.Date(2026, 9, 30, 20, 15, 22, 0, time.UTC)) {
		t.Fatalf("offset not honoured: %+v", lines[1])
	}
	if rejects[0].Line != 4 {
		t.Fatalf("reject line numbers should be 1-based file lines, got %d", rejects[0].Line)
	}
}

func TestPSPAMissingColumn(t *testing.T) {
	_, _, err := readLines(t, PSPA{}, "settlement_id,merchant_ref\nA,B\n")
	if !errors.Is(err, ErrHeader) {
		t.Fatalf("want ErrHeader, got %v", err)
	}
}

func TestBankB(t *testing.T) {
	src := "value_date;txn_datetime;ref_no;description;credit;charges;ccy\n" +
		"02/10/2026;;PO-B-1;PAYOUT 37 TRX 01/10/2026 18:00-24:00;5.512.345,00;61.050,00;IDR\n" +
		"05/10/2026;01/10/2026 13:05:09;ORD-42;QR CREDIT;98.650,00;1.350,00;IDR\n" +
		"05/10/2026;;ORD-43;QR CREDIT;98.650;;IDR\n" + // no charges: fee unknown
		"05/10/2026;;ORD-44;QR CREDIT;-1,00;;IDR\n" + // debit
		"05/10/2026;;PO-X;PAYOUT 3 TRX 01/10/2026 12:00-06:00;1,00;;IDR\n" + // inverted window
		"31/02/2026;;ORD-45;QR CREDIT;1,00;;IDR\n" // impossible date
	lines, rejects, err := readLines(t, BankB{}, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || len(rejects) != 3 {
		t.Fatalf("lines=%d rejects=%d %+v", len(lines), len(rejects), rejects)
	}
	po := lines[0]
	if po.Kind != domain.LinePayout || po.ItemCount != 37 || po.Net != 551_234_500 || po.Fee != 6_105_000 || po.Gross != 557_339_500 {
		t.Fatalf("payout %+v", po)
	}
	if !po.CoversFrom.Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)) || !po.CoversTo.Equal(time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("payout window %v - %v", po.CoversFrom, po.CoversTo)
	}
	item := lines[1]
	if item.Kind != domain.LineItem || item.Reference != "ORD-42" || item.Gross != 10_000_000 || !item.TxnAt.Equal(time.Date(2026, 10, 1, 6, 5, 9, 0, time.UTC)) {
		t.Fatalf("item %+v", item)
	}
	if lines[2].FeeKnown || lines[2].Gross != lines[2].Net {
		t.Fatalf("unknown fee should leave gross == net: %+v", lines[2])
	}
}

// Overlapping statements: the same rows re-exported in another file get the
// same IDs; two identical rows in one file get distinct IDs.
func TestBankBNaturalKeys(t *testing.T) {
	h := "value_date;txn_datetime;ref_no;description;credit;charges;ccy\n"
	row := "05/10/2026;;TRF-1;TRANSFER;10,00;;IDR\n"
	a, _, _ := readLines(t, BankB{}, h+row+row)
	b, _, _ := readLines(t, BankB{}, h+"04/10/2026;;OTHER;X;1,00;;IDR\n"+row)
	if len(a) != 2 || a[0].ID == a[1].ID {
		t.Fatalf("identical rows in one file must stay distinct: %v", a)
	}
	if b[1].ID != a[0].ID {
		t.Fatalf("same row in an overlapping file must keep its key: %s vs %s", b[1].ID, a[0].ID)
	}
}

func TestWriterReaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	pw, _ := NewPSPAWriter(&buf, "psp_a")
	txn := time.Date(2026, 10, 1, 23, 59, 59, 123_000_000, time.UTC)
	id, _ := pw.Write(PSPARow{SettlementID: "S1", MerchantRef: "R,1", TxnTime: txn, SettlementDate: domain.CivilDate(2026, 10, 3), Currency: "IDR", Gross: 12_345_600, Fee: 185_184})
	_ = pw.Flush()
	lines, rej, err := readLines(t, PSPA{}, buf.String())
	if err != nil || len(rej) > 0 || len(lines) != 1 || lines[0].ID != id || lines[0].Reference != "R,1" || !lines[0].TxnAt.Equal(txn) || lines[0].Net != 12_345_600-185_184 {
		t.Fatalf("psp_a round trip: %+v %+v %v", lines, rej, err)
	}

	buf.Reset()
	bw, _ := NewBankBWriter(&buf, "bank_b", wib)
	from := time.Date(2026, 10, 1, 18, 0, 0, 0, wib)
	desc := PayoutDescription(3, from, from.Add(6*time.Hour), wib)
	if desc != "PAYOUT 3 TRX 01/10/2026 18:00-24:00" {
		t.Fatalf("description %q", desc)
	}
	pid, _ := bw.Write(BankBRow{ValueDate: domain.CivilDate(2026, 10, 2), RefNo: "PO-1", Description: desc, Credit: 123_456_789, Charges: 1_000, ChargesKnown: true, Currency: "IDR"})
	iid, _ := bw.Write(BankBRow{ValueDate: domain.CivilDate(2026, 10, 9), TxnTime: txn, RefNo: "ORD-9", Description: "QR CREDIT", Credit: 5_000, Currency: "IDR"})
	_ = bw.Flush()
	lines, rej, err = readLines(t, BankB{}, buf.String())
	if err != nil || len(rej) > 0 || len(lines) != 2 {
		t.Fatalf("bank_b round trip: %+v %+v %v", lines, rej, err)
	}
	if lines[0].ID != pid || lines[0].ItemCount != 3 || !lines[0].CoversFrom.Equal(from) || lines[0].Gross != 123_457_789 {
		t.Fatalf("payout %+v", lines[0])
	}
	if lines[1].ID != iid || !lines[1].TxnAt.Equal(txn.Truncate(time.Second)) {
		t.Fatalf("item %+v", lines[1])
	}
}

func TestReadLedger(t *testing.T) {
	csvSrc := "id,from_account_id,to_account_id,amount,currency,reference,created_at,provider\n" +
		"t1,a,b,150000,IDR,ORD-1,2026-10-01T03:15:22Z,psp_a\n" +
		"t2,a,b,1500.00,IDR,ORD-2,2026-10-01T03:15:22Z,psp_a\n" + // major units: rejected
		"t3,a,b,100,IDR,ORD-3,2026-10-01T03:15:22Z,\n" // provider from default
	jsonArr := `[{"id":"t1","from_account_id":"a","to_account_id":"b","amount":150000,"currency":"IDR","reference":"ORD-1","created_at":"2026-10-01T03:15:22Z","provider":"psp_a"},
	             {"id":"t2","amount":"100","currency":"idr","reference":"ORD-2","created_at":"2026-10-01T03:15:22Z"}]`
	jsonl := `{"id":"t1","amount":150000,"currency":"IDR","reference":"ORD-1","created_at":"2026-10-01T03:15:22Z","provider":"psp_a"}` + "\n\n" +
		`{"id":"t2","amount":-5,"currency":"IDR","reference":"ORD-2","created_at":"2026-10-01T03:15:22Z"}` + "\n" + `{not json}` + "\n"
	cases := []struct {
		name       string
		src        string
		ok, reject int
	}{
		{"csv", csvSrc, 2, 1},
		{"json array", jsonArr, 2, 0},
		{"json lines", jsonl, 1, 2},
		{"json array with BOM", "\ufeff" + jsonArr, 2, 0},
		{"csv with BOM", "\ufeff" + csvSrc, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []domain.LedgerTxn
			var rej []Reject
			err := ReadLedger(strings.NewReader(tc.src), "", Options{Provider: "bank_b"}, func(l domain.LedgerTxn, _ string) error {
				got = append(got, l)
				return nil
			}, func(r Reject) { rej = append(rej, r) })
			if err != nil || len(got) != tc.ok || len(rej) != tc.reject {
				t.Fatalf("got=%d rej=%d err=%v %+v", len(got), len(rej), err, rej)
			}
			if got[0].Amount != 150000 || got[0].Provider != "psp_a" || got[0].Currency != "IDR" {
				t.Fatalf("%+v", got[0])
			}
			if len(got) > 1 && got[1].Provider != "bank_b" {
				t.Fatalf("default provider not applied: %+v", got[1])
			}
		})
	}
}

func TestDecodeEvent(t *testing.T) {
	body := []byte(`{"event_id":"e1","payment_ref":"ORD-1","status":"PAID","amount":1000,"currency":"idr","sequence":2,"occurred_at":"2026-10-01T10:00:00+07:00","unknown":"ok"}`)
	e, err := DecodeEvent(body, "psp_a")
	if err != nil || e.Status != domain.StatusPaid || e.Provider != "psp_a" || e.Currency != "IDR" || e.OccurredAt.Hour() != 3 {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := DecodeEvent([]byte(`{"event_id":"e1","provider":"bank_b","payment_ref":"x","status":"paid","amount":1,"occurred_at":"2026-10-01T10:00:00Z"}`), "psp_a"); err == nil {
		t.Fatal("provider mismatch must be rejected")
	}
	if _, err := DecodeEvent([]byte(`{"event_id":"e1","payment_ref":"x","status":"captured","amount":1,"occurred_at":"2026-10-01T10:00:00Z"}`), "psp_a"); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatal("unknown status must be rejected")
	}
}

// ---- fuzz: adapters must never panic, and whatever they accept must satisfy
// the invariants the matcher relies on ----

func FuzzPSPA(f *testing.F) {
	f.Add("STL-1,ORD-1,2026-10-01T03:15:22Z,2026-10-03,IDR,150000.00,2250.00,147750.00")
	f.Add("S,\"R\"\"1\",2026-10-01T03:15:22Z,2026-10-03,JPY,1,0,1")
	f.Fuzz(func(t *testing.T, row string) {
		src := strings.Join(pspaColumns, ",") + "\n" + row + "\n"
		_ = PSPA{}.Read(strings.NewReader(src), Options{Provider: "psp_a"}, func(l domain.SettlementLine, _ string) error {
			if l.Gross-l.Fee != l.Net || l.Gross <= 0 || l.Fee < 0 || !l.Currency.Valid() || l.ID == "" {
				t.Fatalf("invariant broken: %+v", l)
			}
			return nil
		}, func(Reject) {})
	})
}

func FuzzBankB(f *testing.F) {
	f.Add("02/10/2026;;PO-B-1;PAYOUT 37 TRX 01/10/2026 06:00-12:00;5.512.345,00;61.050,00;IDR")
	f.Add("05/10/2026;01/10/2026 13:05:09;ORD-42;QR CREDIT;98.650,00;;IDR")
	f.Fuzz(func(t *testing.T, row string) {
		src := strings.Join(bankbColumns, ";") + "\n" + row + "\n"
		_ = BankB{}.Read(strings.NewReader(src), Options{Provider: "bank_b", Location: wib}, func(l domain.SettlementLine, _ string) error {
			if l.Net <= 0 || (l.FeeKnown && l.Gross != l.Net+l.Fee) || (!l.FeeKnown && l.Gross != l.Net) {
				t.Fatalf("amount invariant broken: %+v", l)
			}
			if l.Kind == domain.LinePayout && (l.ItemCount <= 0 || !l.CoversTo.After(l.CoversFrom)) {
				t.Fatalf("payout invariant broken: %+v", l)
			}
			return nil
		}, func(Reject) {})
	})
}

func FuzzLedger(f *testing.F) {
	f.Add("t1,a,b,150000,IDR,ORD-1,2026-10-01T03:15:22Z,psp_a")
	f.Add(`{"id":"t1","amount":1,"currency":"IDR","reference":"r","created_at":"2026-10-01T03:15:22Z"}`)
	f.Fuzz(func(t *testing.T, row string) {
		for _, src := range []string{strings.Join(ledgerColumns, ",") + "\n" + row, row} {
			_ = ReadLedger(strings.NewReader(src), "", Options{Provider: "p"}, func(l domain.LedgerTxn, _ string) error {
				if l.Amount <= 0 || !domain.ValidAmount(l.Amount) || l.ID == "" || l.Provider == "" || !l.Currency.Valid() {
					t.Fatalf("invariant broken: %+v", l)
				}
				return nil
			}, func(Reject) {})
		}
	})
}

func FuzzDecodeEvent(f *testing.F) {
	f.Add([]byte(`{"event_id":"e1","payment_ref":"ORD-1","status":"paid","amount":1000,"currency":"IDR","sequence":2,"occurred_at":"2026-10-01T10:00:00Z"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := DecodeEvent(b, "psp_a")
		if err == nil && (e.Validate() != nil || e.Provider != "psp_a") {
			t.Fatalf("accepted invalid event %+v", e)
		}
	})
}
