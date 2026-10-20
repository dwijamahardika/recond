package ingest

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Ledger exports follow a ledgerd-style transfer shape: id, from_account_id,
// to_account_id, amount (integer minor units), currency, reference,
// created_at (RFC 3339) - plus provider, the payment provider the transfer was
// posted against. Files without a provider column use Options.Provider.
var ledgerColumns = []string{"id", "from_account_id", "to_account_id", "amount", "currency", "reference", "created_at", "provider"}

type ledgerWire struct {
	ID          string          `json:"id"`
	FromAccount string          `json:"from_account_id"`
	ToAccount   string          `json:"to_account_id"`
	Amount      json.RawMessage `json:"amount"`
	Currency    string          `json:"currency"`
	Reference   string          `json:"reference"`
	CreatedAt   string          `json:"created_at"`
	Provider    string          `json:"provider"`
}

// ReadLedger reads a ledger export. format is "csv", "json" (a JSON array or
// JSON Lines) or "" to detect from the first non-blank byte.
func ReadLedger(r io.Reader, format string, opt Options, emit Emit[domain.LedgerTxn], reject func(Reject)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	if b, err := br.Peek(3); err == nil && string(b) == "\ufeff" { // UTF-8 BOM from spreadsheet exports
		_, _ = br.Discard(3)
	}
	if format == "" {
		format = "csv"
		if b, err := peekNonSpace(br); err == nil && (b == '[' || b == '{') {
			format = "json"
		}
	}
	switch format {
	case "csv":
		return readLedgerCSV(br, opt, emit, reject)
	case "json", "jsonl":
		return readLedgerJSON(br, opt, emit, reject)
	}
	return fmt.Errorf("unknown ledger format %q", format)
}

func peekNonSpace(br *bufio.Reader) (byte, error) {
	for i := 1; ; i++ {
		b, err := br.Peek(i)
		if err != nil {
			return 0, err
		}
		if c := b[i-1]; c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return c, nil
		}
	}
}

func readLedgerCSV(r io.Reader, opt Options, emit Emit[domain.LedgerTxn], reject func(Reject)) error {
	cr := newCSVReader(r, ',')
	var idx map[string]int
	return eachRow(cr, ',', func(h []string) (err error) {
		idx, err = columns(h, "id", "amount", "currency", "reference", "created_at")
		return err
	}, func(line int, rec []string, raw string) error {
		w := ledgerWire{
			ID: field(rec, idx, "id"), FromAccount: field(rec, idx, "from_account_id"), ToAccount: field(rec, idx, "to_account_id"),
			Amount: json.RawMessage(field(rec, idx, "amount")), Currency: field(rec, idx, "currency"),
			Reference: field(rec, idx, "reference"), CreatedAt: field(rec, idx, "created_at"), Provider: field(rec, idx, "provider"),
		}
		return ledgerRow(line, raw, w, opt, emit, reject)
	}, reject)
}

func readLedgerJSON(br *bufio.Reader, opt Options, emit Emit[domain.LedgerTxn], reject func(Reject)) error {
	first, err := peekNonSpace(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if first == '[' {
		dec := json.NewDecoder(br)
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("ledger json: %w", err)
		}
		for n := 1; dec.More(); n++ {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return fmt.Errorf("ledger json element %d: %w", n, err)
			}
			if err := ledgerJSONRow(n, raw, opt, emit, reject); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		if err := ledgerJSONRow(line, append([]byte(nil), b...), opt, emit, reject); err != nil {
			return err
		}
	}
	return sc.Err()
}

func ledgerJSONRow(line int, raw []byte, opt Options, emit Emit[domain.LedgerTxn], reject func(Reject)) error {
	var w ledgerWire
	if err := json.Unmarshal(raw, &w); err != nil {
		reject(Reject{Line: line, Reason: "malformed json: " + err.Error(), Raw: truncate(string(raw))})
		return nil
	}
	return ledgerRow(line, string(raw), w, opt, emit, reject)
}

func ledgerRow(line int, raw string, w ledgerWire, opt Options, emit Emit[domain.LedgerTxn], reject func(Reject)) error {
	bad := func(reason string) error {
		reject(Reject{Line: line, Reason: reason, Raw: truncate(raw)})
		return nil
	}
	t := domain.LedgerTxn{
		ID: strings.TrimSpace(w.ID), Provider: strings.TrimSpace(w.Provider), Reference: strings.TrimSpace(w.Reference),
		Currency: domain.Currency(strings.ToUpper(strings.TrimSpace(w.Currency))), FromAccount: w.FromAccount, ToAccount: w.ToAccount,
	}
	if t.Provider == "" {
		t.Provider = opt.Provider
	}
	switch {
	case t.ID == "" || len(t.ID) > 128:
		return bad("id required (max 128 chars)")
	case t.Provider == "":
		return bad("provider required (column or -provider default)")
	case len(t.Reference) > 200:
		return bad("reference max 200 chars")
	case !t.Currency.Valid():
		return bad(fmt.Sprintf("unsupported currency %q", w.Currency))
	}
	// Amounts are integer minor units, as ledgerd stores them. A JSON string is
	// tolerated; a decimal point is not - that would mean someone exported
	// major units and we would be off by 100x.
	amt, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(string(w.Amount)), `"`), 10, 64)
	if err != nil || amt <= 0 || !domain.ValidAmount(amt) {
		return bad(fmt.Sprintf("amount must be a positive integer of minor units, got %q", string(w.Amount)))
	}
	t.Amount = amt
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(w.CreatedAt))
	if err != nil {
		return bad("created_at must be RFC 3339")
	}
	t.CreatedAt = ts.UTC()
	return emit(t, RowHash(t.ID, t.Provider, t.Reference, strconv.FormatInt(t.Amount, 10), string(t.Currency),
		strconv.FormatInt(t.CreatedAt.UnixNano(), 10), t.FromAccount, t.ToAccount))
}

func truncate(s string) string {
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

// LedgerCSVWriter writes the CSV ledger export format.
type LedgerCSVWriter struct{ w *csv.Writer }

// NewLedgerCSVWriter writes the header and returns a writer.
func NewLedgerCSVWriter(w io.Writer) (*LedgerCSVWriter, error) {
	cw := csv.NewWriter(w)
	return &LedgerCSVWriter{w: cw}, cw.Write(ledgerColumns)
}

func (l *LedgerCSVWriter) Write(t domain.LedgerTxn) error {
	return l.w.Write([]string{t.ID, t.FromAccount, t.ToAccount, strconv.FormatInt(t.Amount, 10), string(t.Currency),
		t.Reference, t.CreatedAt.UTC().Format(time.RFC3339Nano), t.Provider})
}

// Flush flushes buffered rows.
func (l *LedgerCSVWriter) Flush() error { l.w.Flush(); return l.w.Error() }
