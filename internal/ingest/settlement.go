package ingest

import (
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Adapter parses one provider settlement format into normalised lines.
type Adapter interface {
	Format() string
	Read(r io.Reader, opt Options, emit Emit[domain.SettlementLine], reject func(Reject)) error
}

var adapters = map[string]Adapter{
	"psp_a":  PSPA{},
	"bank_b": BankB{},
}

// AdapterFor returns the adapter for a settlement format name.
func AdapterFor(format string) (Adapter, bool) {
	a, ok := adapters[format]
	return a, ok
}

// Formats lists the registered settlement formats.
func Formats() []string {
	out := make([]string, 0, len(adapters))
	for k := range adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// psp_a: a fictional payment service provider that sends one itemised
// settlement report per settlement date. Comma-separated, header row, amounts
// in major units with '.' decimals and no grouping, RFC 3339 UTC timestamps,
// ISO dates, and an explicit gross/fee/net per transaction.
//
//	settlement_id,merchant_ref,transaction_time,settlement_date,currency,gross_amount,fee_amount,net_amount
//	STL-A-00000001,ORD-20261001-0000001,2026-10-01T03:15:22Z,2026-10-03,IDR,150000.00,2250.00,147750.00
// ---------------------------------------------------------------------------

// PSPA parses psp_a settlement reports.
type PSPA struct{}

var pspaColumns = []string{"settlement_id", "merchant_ref", "transaction_time", "settlement_date", "currency", "gross_amount", "fee_amount", "net_amount"}

// Format returns the format name.
func (PSPA) Format() string { return "psp_a" }

func (PSPA) Read(r io.Reader, opt Options, emit Emit[domain.SettlementLine], reject func(Reject)) error {
	cr := newCSVReader(r, ',')
	var idx map[string]int
	return eachRow(cr, ',', func(h []string) (err error) {
		idx, err = columns(h, pspaColumns...)
		return err
	}, func(line int, rec []string, raw string) error {
		bad := func(format string, args ...any) error {
			reject(Reject{Line: line, Reason: fmt.Sprintf(format, args...), Raw: truncate(raw)})
			return nil
		}
		sid := field(rec, idx, "settlement_id")
		if sid == "" || len(sid) > 128 {
			return bad("settlement_id required (max 128)")
		}
		cur := domain.Currency(strings.ToUpper(field(rec, idx, "currency")))
		if !cur.Valid() {
			return bad("unsupported currency %q", field(rec, idx, "currency"))
		}
		exp := cur.Exponent()
		var amts [3]int64
		for i, col := range []string{"gross_amount", "fee_amount", "net_amount"} {
			v, err := domain.ParseAmount(field(rec, idx, col), domain.NumberFormat{Decimal: '.'}, exp)
			if err != nil {
				return bad("%s: %v", col, err)
			}
			amts[i] = v
		}
		gross, fee, net := amts[0], amts[1], amts[2]
		switch {
		case gross <= 0 || fee < 0:
			return bad("negative or zero amounts (adjustments) are not supported")
		case gross-fee != net:
			return bad("net %d != gross %d - fee %d", net, gross, fee)
		}
		txnAt, err := time.Parse(time.RFC3339Nano, field(rec, idx, "transaction_time"))
		if err != nil {
			return bad("transaction_time must be RFC 3339")
		}
		settled, err := domain.ParseDate("2006-01-02", field(rec, idx, "settlement_date"))
		if err != nil {
			return bad("settlement_date must be YYYY-MM-DD")
		}
		l := domain.SettlementLine{
			ID: opt.Provider + ":" + sid, Provider: opt.Provider, Kind: domain.LineItem,
			Reference: field(rec, idx, "merchant_ref"), Currency: cur,
			Gross: gross, Fee: fee, Net: net, FeeKnown: true,
			TxnAt: txnAt.UTC(), SettledOn: settled, SourceLine: line, Raw: raw,
		}
		return emit(l, RowHash(sid, l.Reference, strconv.FormatInt(l.TxnAt.UnixNano(), 10), settled.String(), string(cur),
			strconv.FormatInt(gross, 10), strconv.FormatInt(fee, 10)))
	}, reject)
}

// PSPARow is one psp_a report row, for writers.
type PSPARow struct {
	SettlementID   string
	MerchantRef    string
	TxnTime        time.Time
	SettlementDate domain.Date
	Currency       domain.Currency
	Gross, Fee     int64
}

// PSPAWriter writes psp_a reports.
type PSPAWriter struct {
	w        *csv.Writer
	provider string
}

// NewPSPAWriter writes the header and returns a writer.
func NewPSPAWriter(w io.Writer, provider string) (*PSPAWriter, error) {
	cw := csv.NewWriter(w)
	return &PSPAWriter{w: cw, provider: provider}, cw.Write(pspaColumns)
}

// Write emits a row and returns the line ID the reader will assign to it.
func (p *PSPAWriter) Write(r PSPARow) (string, error) {
	exp := r.Currency.Exponent()
	f := domain.NumberFormat{Decimal: '.'}
	err := p.w.Write([]string{r.SettlementID, r.MerchantRef, r.TxnTime.UTC().Format(time.RFC3339Nano), r.SettlementDate.String(), string(r.Currency),
		domain.FormatAmount(r.Gross, exp, f, false), domain.FormatAmount(r.Fee, exp, f, false), domain.FormatAmount(r.Gross-r.Fee, exp, f, false)})
	return p.provider + ":" + r.SettlementID, err
}

// Flush flushes buffered rows.
func (p *PSPAWriter) Flush() error { p.w.Flush(); return p.w.Error() }

// ---------------------------------------------------------------------------
// bank_b: a fictional acquiring bank's statement export. Semicolon-separated,
// Indonesian number format (1.500.000,00), DD/MM/YYYY dates and local
// wall-clock times without an offset (the provider's timezone, UTC+7 in the
// example config). Credits are NET of charges. Most money arrives as payouts
// that cover a settlement cycle and are not itemised - only the description
// says how many transactions they cover and for which window:
//
//	value_date;txn_datetime;ref_no;description;credit;charges;ccy
//	02/10/2026;;PO-B-20261001-1;PAYOUT 37 TRX 01/10/2026 06:00-12:00;5.512.345,00;61.050,00;IDR
//	05/10/2026;01/10/2026 13:05:09;ORD-20261001-0000042;QR CREDIT;98.650,00;1.350,00;IDR
//
// Bank statements carry no row id, so the natural key is a hash of the row's
// content plus its occurrence index within the file: re-ingesting an
// overlapping statement is idempotent, while two genuinely identical rows in
// one statement are both kept.
// ---------------------------------------------------------------------------

// BankB parses bank_b statements.
type BankB struct{}

var (
	bankbColumns  = []string{"value_date", "txn_datetime", "ref_no", "description", "credit", "charges", "ccy"}
	payoutPattern = regexp.MustCompile(`^PAYOUT (\d+) TRX (\d{2}/\d{2}/\d{4}) (\d{2}):(\d{2})-(\d{2}):(\d{2})$`)
)

const (
	bankbDate     = "02/01/2006"
	bankbDateTime = "02/01/2006 15:04:05"
)

// Format returns the format name.
func (BankB) Format() string { return "bank_b" }

func (BankB) Read(r io.Reader, opt Options, emit Emit[domain.SettlementLine], reject func(Reject)) error {
	loc := opt.Location
	if loc == nil {
		loc = time.UTC
	}
	cr := newCSVReader(r, ';')
	var idx map[string]int
	seen := map[string]int{}
	return eachRow(cr, ';', func(h []string) (err error) {
		idx, err = columns(h, bankbColumns...)
		return err
	}, func(line int, rec []string, raw string) error {
		bad := func(format string, args ...any) error {
			reject(Reject{Line: line, Reason: fmt.Sprintf(format, args...), Raw: truncate(raw)})
			return nil
		}
		vals := make([]string, len(bankbColumns))
		for i, c := range bankbColumns {
			vals[i] = field(rec, idx, c)
		}
		hash := RowHash(vals...)
		seen[hash]++
		id := fmt.Sprintf("%s:%s#%d", opt.Provider, hash[:20], seen[hash])

		cur := domain.Currency(strings.ToUpper(vals[6]))
		if !cur.Valid() {
			return bad("unsupported currency %q", vals[6])
		}
		exp := cur.Exponent()
		valueDate, err := domain.ParseDate(bankbDate, vals[0])
		if err != nil {
			return bad("value_date must be DD/MM/YYYY")
		}
		credit, err := domain.ParseAmount(vals[4], domain.CommaDecimal, exp)
		if err != nil {
			return bad("credit: %v", err)
		}
		if credit <= 0 {
			return bad("debits and zero credits are not supported")
		}
		l := domain.SettlementLine{
			ID: id, Provider: opt.Provider, Kind: domain.LineItem, Reference: vals[2], Currency: cur,
			Net: credit, Gross: credit, SettledOn: valueDate, SourceLine: line, Raw: raw,
		}
		if vals[5] != "" {
			fee, err := domain.ParseAmount(vals[5], domain.CommaDecimal, exp)
			if err != nil || fee < 0 {
				return bad("charges: invalid %q", vals[5])
			}
			gross, ok := domain.AddChecked(credit, fee)
			if !ok || !domain.ValidAmount(gross) {
				return bad("credit + charges out of range")
			}
			l.Fee, l.Gross, l.FeeKnown = fee, gross, true
		}
		if vals[1] != "" {
			ts, err := time.ParseInLocation(bankbDateTime, vals[1], loc)
			if err != nil {
				return bad("txn_datetime must be DD/MM/YYYY HH:MM:SS")
			}
			l.TxnAt = ts.UTC()
		}
		if m := payoutPattern.FindStringSubmatch(vals[3]); m != nil {
			n, _ := strconv.Atoi(m[1])
			day, err := time.ParseInLocation(bankbDate, m[2], loc)
			if err != nil || n <= 0 {
				return bad("payout description: bad date or count")
			}
			from, okFrom := clock(day, m[3], m[4])
			to, okTo := clock(day, m[5], m[6])
			if !okFrom || !okTo || !to.After(from) {
				return bad("payout description: bad window")
			}
			l.Kind, l.ItemCount, l.CoversFrom, l.CoversTo = domain.LinePayout, n, from.UTC(), to.UTC()
		}
		return emit(l, hash)
	}, reject)
}

// clock returns day at hh:mm; "24:00" is the end of the day.
func clock(day time.Time, hh, mm string) (time.Time, bool) {
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || h > 24 || m > 59 || (h == 24 && m != 0) {
		return time.Time{}, false
	}
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), true
}

// BankBRow is one statement row, for writers.
type BankBRow struct {
	ValueDate    domain.Date
	TxnTime      time.Time // zero: empty column
	RefNo        string
	Description  string
	Credit       int64
	Charges      int64
	ChargesKnown bool
	Currency     domain.Currency
}

// BankBWriter writes bank_b statements.
type BankBWriter struct {
	w        *csv.Writer
	provider string
	loc      *time.Location
	seen     map[string]int
}

// NewBankBWriter writes the header and returns a writer.
func NewBankBWriter(w io.Writer, provider string, loc *time.Location) (*BankBWriter, error) {
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	return &BankBWriter{w: cw, provider: provider, loc: loc, seen: map[string]int{}}, cw.Write(bankbColumns)
}

// Write emits a row and returns the line ID the reader will assign to it.
func (b *BankBWriter) Write(r BankBRow) (string, error) {
	exp := r.Currency.Exponent()
	vals := []string{r.ValueDate.Format(bankbDate), "", r.RefNo, r.Description, domain.FormatAmount(r.Credit, exp, domain.CommaDecimal, true), "", string(r.Currency)}
	if !r.TxnTime.IsZero() {
		vals[1] = r.TxnTime.In(b.loc).Format(bankbDateTime)
	}
	if r.ChargesKnown {
		vals[5] = domain.FormatAmount(r.Charges, exp, domain.CommaDecimal, true)
	}
	hash := RowHash(vals...)
	b.seen[hash]++
	return fmt.Sprintf("%s:%s#%d", b.provider, hash[:20], b.seen[hash]), b.w.Write(vals)
}

// Flush flushes buffered rows.
func (b *BankBWriter) Flush() error { b.w.Flush(); return b.w.Error() }

// PayoutDescription renders the description bank_b uses for a payout covering
// [from, to) in loc. to may be the following midnight ("24:00").
func PayoutDescription(count int, from, to time.Time, loc *time.Location) string {
	from, to = from.In(loc), to.In(loc)
	end := to.Format("15:04")
	if y1, m1, d1 := from.Date(); to.Hour() == 0 && to.Minute() == 0 {
		if y2, m2, d2 := to.Date(); y1 != y2 || m1 != m2 || d1 != d2 {
			end = "24:00"
		}
	}
	return fmt.Sprintf("PAYOUT %d TRX %s %s-%s", count, from.Format(bankbDate), from.Format("15:04"), end)
}
