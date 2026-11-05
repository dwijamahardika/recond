package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

// Windowed runs read through the indexes migration 0002 adds: ledger by
// booking time, lines by coalesce(txn_at, covers_from), untimed lines by
// settled_on. The bounds are a superset of the window's visible range (the
// widest instants over every provider's timezone); the engine applies the
// exact per-provider rule, so what is read here only has to be enough.

const (
	ledgerCols = `id, provider, reference, amount, currency, created_at`
	lineCols   = `id, provider, kind, reference, currency, gross, fee, net, fee_known, txn_at, settled_on,
		covers_from, covers_to, item_count, source_line`

	paymentsForSQL = `SELECT ` + paymentCols + ` FROM payments WHERE (provider, payment_ref) IN (VALUES `
	foldForSQL     = `SELECT ` + eventCols + ` FROM webhook_events WHERE seq <= ? AND (provider, payment_ref) IN (VALUES `
	ledgerByRefSQL = `SELECT ` + ledgerCols + `, upper(trim(reference)) FROM ledger_txns WHERE provider = ? AND upper(trim(reference)) IN (`
	linesByRefSQL  = `SELECT ` + lineCols + `, upper(trim(reference)) FROM settlement_lines WHERE provider = ? AND kind = 'item' AND upper(trim(reference)) IN (`
)

// LoadWindow reads the records inside b and the payment states of their
// references, from one consistent snapshot, for a new windowed run.
func (s *Store) LoadWindow(ctx context.Context, b match.LoadBounds) (*Inputs, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	in := &Inputs{}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM ingest_files`).Scan(&in.MaxFileID); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM webhook_events`).Scan(&in.MaxEventSeq); err != nil {
		return nil, err
	}
	if err := loadWindowRecords(ctx, tx, in, b); err != nil {
		return nil, err
	}
	in.Payments, err = paymentsFor(ctx, tx, referenced(in))
	return in, err
}

// LoadWindowAt reads the records inside b as of past watermarks, with
// payment states folded from the event log up to maxSeq (for verify).
func (s *Store) LoadWindowAt(ctx context.Context, maxFile, maxSeq int64, b match.LoadBounds) (*Inputs, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	in := &Inputs{MaxFileID: maxFile, MaxEventSeq: maxSeq}
	if err := loadWindowRecords(ctx, tx, in, b); err != nil {
		return nil, err
	}
	in.Payments, err = foldPaymentsFor(ctx, tx, maxSeq, referenced(in))
	return in, err
}

func loadWindowRecords(ctx context.Context, tx *sql.Tx, in *Inputs, b match.LoadBounds) error {
	var err error
	if in.Digest, err = inputDigest(ctx, tx, in.MaxFileID, in.MaxEventSeq); err != nil {
		return err
	}
	from, to := b.From.UnixNano(), b.To.UnixNano()
	in1 := interner{}
	if in.Ledger, err = scanLedger(ctx, tx, in1, `SELECT `+ledgerCols+` FROM ledger_txns
		WHERE created_at >= ? AND created_at < ? AND file_id <= ?`, from, to, in.MaxFileID); err != nil {
		return err
	}
	if in.Lines, err = scanLines(ctx, tx, in1, nil, `SELECT `+lineCols+` FROM settlement_lines
		WHERE coalesce(txn_at, covers_from) >= ? AND coalesce(txn_at, covers_from) < ? AND file_id <= ?`, from, to, in.MaxFileID); err != nil {
		return err
	}
	in.Lines, err = scanLines(ctx, tx, in1, in.Lines, `SELECT `+lineCols+` FROM settlement_lines
		WHERE txn_at IS NULL AND covers_from IS NULL AND settled_on BETWEEN ? AND ? AND file_id <= ?`,
		b.UntimedFrom.String(), b.UntimedTo.String(), in.MaxFileID)
	return err
}

func scanLedger(ctx context.Context, q querier, in1 interner, query string, args ...any) ([]domain.LedgerTxn, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.LedgerTxn
	for rows.Next() {
		var t domain.LedgerTxn
		var cur, prov string
		var at int64
		if err := rows.Scan(&t.ID, &prov, &t.Reference, &t.Amount, &cur, &at); err != nil {
			return nil, err
		}
		t.Provider, t.Currency, t.CreatedAt = in1.get(prov), domain.Currency(in1.get(cur)), fromNanos(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanLines(ctx context.Context, q querier, in1 interner, out []domain.SettlementLine, query string, args ...any) ([]domain.SettlementLine, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		l, err := scanLine(rows, in1)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLine(rows *sql.Rows, in1 interner, extra ...any) (domain.SettlementLine, error) {
	var l domain.SettlementLine
	var prov, kind, cur, settled string
	var feeKnown int
	var txnAt, from, to sql.NullInt64
	dest := append([]any{&l.ID, &prov, &kind, &l.Reference, &cur, &l.Gross, &l.Fee, &l.Net, &feeKnown, &txnAt, &settled,
		&from, &to, &l.ItemCount, &l.SourceLine}, extra...)
	if err := rows.Scan(dest...); err != nil {
		return l, err
	}
	l.Provider, l.Kind, l.Currency, l.FeeKnown = in1.get(prov), domain.LineKind(in1.get(kind)), domain.Currency(in1.get(cur)), feeKnown == 1
	l.TxnAt, l.CoversFrom, l.CoversTo = fromNull(txnAt), fromNull(from), fromNull(to)
	return l, l.SettledOn.UnmarshalText([]byte(settled))
}

// referenced returns the payment keys of loaded ledger rows and itemised
// lines, sorted: the only payments a windowed run (and its evidence) uses.
func referenced(in *Inputs) []domain.PaymentKey {
	set := map[domain.PaymentKey]bool{}
	for i := range in.Ledger {
		set[domain.PaymentKey{Provider: in.Ledger[i].Provider, Ref: in.Ledger[i].Reference}] = true
	}
	for i := range in.Lines {
		if in.Lines[i].Kind == domain.LineItem {
			set[domain.PaymentKey{Provider: in.Lines[i].Provider, Ref: in.Lines[i].Reference}] = true
		}
	}
	keys := make([]domain.PaymentKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		return keys[i].Ref < keys[j].Ref
	})
	return keys
}

// keyChunks calls fn with row-value placeholders "(?, ?), ..." for chunks
// of keys, staying well under SQLite's bound-parameter limit.
func keyChunks(keys []domain.PaymentKey, fn func(values string, args []any) error) error {
	const size = 400
	for start := 0; start < len(keys); start += size {
		chunk := keys[start:min(start+size, len(keys))]
		args := make([]any, 0, 2*len(chunk))
		for _, k := range chunk {
			args = append(args, k.Provider, k.Ref)
		}
		if err := fn(strings.TrimSuffix(strings.Repeat("(?, ?),", len(chunk)), ","), args); err != nil {
			return err
		}
	}
	return nil
}

// paymentsFor reads the payments projection for keys.
func paymentsFor(ctx context.Context, tx *sql.Tx, keys []domain.PaymentKey) (map[domain.PaymentKey]domain.Payment, error) {
	out := make(map[domain.PaymentKey]domain.Payment, len(keys))
	err := keyChunks(keys, func(values string, args []any) error {
		query := paymentsForSQL + values + ")" // values holds only "(?, ?)" placeholders
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		return scanPaymentRows(rows, out)
	})
	return out, err
}

// foldPaymentsFor rebuilds the states of keys from the event log up to maxSeq.
func foldPaymentsFor(ctx context.Context, tx *sql.Tx, maxSeq int64, keys []domain.PaymentKey) (map[domain.PaymentKey]domain.Payment, error) {
	out := make(map[domain.PaymentKey]domain.Payment, len(keys))
	err := keyChunks(keys, func(values string, args []any) error {
		query := foldForSQL + values + ") ORDER BY provider, payment_ref, seq" // values holds only "(?, ?)" placeholders
		rows, err := tx.QueryContext(ctx, query, append([]any{maxSeq}, args...)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var cur domain.PaymentKey
		var batch []domain.PaymentEvent
		flush := func() {
			if len(batch) > 0 {
				p, _ := domain.Fold(batch)
				out[cur] = p
			}
			batch = batch[:0]
		}
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			if k := (domain.PaymentKey{Provider: e.Provider, Ref: e.PaymentRef}); k != cur {
				flush()
				cur = k
			}
			batch = append(batch, e)
		}
		flush()
		return rows.Err()
	})
	return out, err
}

// ---- counterpart lookups ----

// maxPayoutSpan bounds how far back a payout covering an instant may start;
// bank_b payouts cover at most a day.
const maxPayoutSpan = 31 * 24 * time.Hour

// Prober answers a windowed run's counterpart lookups from the database as
// of file watermark maxFile. Inputs are append-only, so the answers for a
// watermark never change and a replay gets the same ones.
func (s *Store) Prober(ctx context.Context, maxFile int64) match.Prober {
	return &prober{ctx: ctx, db: s.db, maxFile: maxFile}
}

type prober struct {
	ctx     context.Context // the engine's Prober interface is context-free; a prober lives for one run
	db      *sql.DB
	maxFile int64
}

func strChunks(keys []string, fn func(placeholders string, args []any) error) error {
	const size = 500
	for start := 0; start < len(keys); start += size {
		chunk := keys[start:min(start+size, len(keys))]
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		if err := fn(strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ","), args); err != nil {
			return err
		}
	}
	return nil
}

// LedgerByRef implements match.Prober through ledger_txns_ref.
func (p *prober) LedgerByRef(provider string, keys []string) (map[string][]domain.LedgerTxn, error) {
	out := map[string][]domain.LedgerTxn{}
	in1 := interner{}
	err := strChunks(keys, func(ph string, args []any) error {
		query := ledgerByRefSQL + ph + ") AND file_id <= ? ORDER BY id" // ph holds only "?" placeholders
		rows, err := p.db.QueryContext(p.ctx, query, append(append([]any{provider}, args...), p.maxFile)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var t domain.LedgerTxn
			var cur, prov, key string
			var at int64
			if err := rows.Scan(&t.ID, &prov, &t.Reference, &t.Amount, &cur, &at, &key); err != nil {
				return err
			}
			t.Provider, t.Currency, t.CreatedAt = in1.get(prov), domain.Currency(in1.get(cur)), fromNanos(at)
			out[key] = append(out[key], t)
		}
		return rows.Err()
	})
	return out, err
}

// LinesByRef implements match.Prober through settlement_lines_item_ref.
func (p *prober) LinesByRef(provider string, keys []string) (map[string][]domain.SettlementLine, error) {
	out := map[string][]domain.SettlementLine{}
	in1 := interner{}
	err := strChunks(keys, func(ph string, args []any) error {
		query := linesByRefSQL + ph + ") AND file_id <= ? ORDER BY id" // ph holds only "?" placeholders
		rows, err := p.db.QueryContext(p.ctx, query, append(append([]any{provider}, args...), p.maxFile)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key string
			l, err := scanLine(rows, in1, &key)
			if err != nil {
				return err
			}
			out[key] = append(out[key], l)
		}
		return rows.Err()
	})
	return out, err
}

// PayoutsCovering implements match.Prober through settlement_lines_payout.
func (p *prober) PayoutsCovering(provider string, at time.Time) ([]domain.SettlementLine, error) {
	t := at.UnixNano()
	return scanLines(p.ctx, p.db, interner{}, nil, `SELECT `+lineCols+` FROM settlement_lines
		WHERE provider = ? AND kind = 'payout' AND covers_from <= ? AND covers_from >= ? AND covers_to > ? AND file_id <= ? ORDER BY id`,
		provider, t, at.Add(-maxPayoutSpan).UnixNano(), t, p.maxFile)
}
