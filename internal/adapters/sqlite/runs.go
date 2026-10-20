package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

// Inputs is everything a run reconciles, read from one consistent snapshot.
type Inputs struct {
	MaxFileID   int64
	MaxEventSeq int64
	Digest      string // identifies the input set: file hashes + event watermark
	Ledger      []domain.LedgerTxn
	Lines       []domain.SettlementLine
	Payments    map[domain.PaymentKey]domain.Payment
}

// LoadInputs reads everything ingested so far. Payment states come from the
// payments projection, which in the same snapshot reflects exactly the events
// up to the event watermark.
func (s *Store) LoadInputs(ctx context.Context) (*Inputs, error) {
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
	if err := loadRecords(ctx, tx, in); err != nil {
		return nil, err
	}
	if in.Payments, err = scanPayments(ctx, tx, `SELECT `+paymentCols+` FROM payments`); err != nil {
		return nil, err
	}
	return in, nil
}

// LoadInputsAt reads the inputs as of past watermarks: records from files up
// to maxFile and payment states folded from events up to maxSeq.
func (s *Store) LoadInputsAt(ctx context.Context, maxFile, maxSeq int64) (*Inputs, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	in := &Inputs{MaxFileID: maxFile, MaxEventSeq: maxSeq}
	if err := loadRecords(ctx, tx, in); err != nil {
		return nil, err
	}
	if in.Payments, err = foldPaymentsAt(ctx, tx, maxSeq); err != nil {
		return nil, err
	}
	return in, nil
}

// interner shares the few distinct provider/currency strings across a
// million rows instead of allocating each one.
type interner map[string]string

func (m interner) get(s string) string {
	if v, ok := m[s]; ok {
		return v
	}
	m[s] = s
	return s
}

func loadRecords(ctx context.Context, tx *sql.Tx, in *Inputs) error {
	var err error
	if in.Digest, err = inputDigest(ctx, tx, in.MaxFileID, in.MaxEventSeq); err != nil {
		return err
	}
	in1 := interner{}
	if in.Ledger, err = loadLedger(ctx, tx, in.MaxFileID, in1); err != nil {
		return err
	}
	in.Lines, err = loadLines(ctx, tx, in.MaxFileID, in1)
	return err
}

// inputDigest identifies an input set: every file (id and content hash) up
// to the file watermark, plus the event watermark.
func inputDigest(ctx context.Context, tx *sql.Tx, maxFile, maxSeq int64) (string, error) {
	h := sha256.New()
	rows, err := tx.QueryContext(ctx, `SELECT id, sha256 FROM ingest_files WHERE id <= ? ORDER BY id`, maxFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%d:%s\n", id, sum)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(h, "events<=%d\n", maxSeq)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadLedger(ctx context.Context, tx *sql.Tx, maxFile int64, in1 interner) ([]domain.LedgerTxn, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ledger_txns WHERE file_id <= ?`, maxFile).Scan(&n); err != nil {
		return nil, err
	}
	out := make([]domain.LedgerTxn, 0, n)
	// Account columns are evidence-only and are fetched per discrepancy.
	rows, err := tx.QueryContext(ctx, `SELECT id, provider, reference, amount, currency, created_at FROM ledger_txns WHERE file_id <= ?`, maxFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

func loadLines(ctx context.Context, tx *sql.Tx, maxFile int64, in1 interner) ([]domain.SettlementLine, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM settlement_lines WHERE file_id <= ?`, maxFile).Scan(&n); err != nil {
		return nil, err
	}
	out := make([]domain.SettlementLine, 0, n)
	rows, err := tx.QueryContext(ctx, `SELECT id, provider, kind, reference, currency, gross, fee, net, fee_known, txn_at, settled_on,
		covers_from, covers_to, item_count, source_line FROM settlement_lines WHERE file_id <= ?`, maxFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var l domain.SettlementLine
		var prov, kind, cur, settled string
		var feeKnown int
		var txnAt, from, to sql.NullInt64
		if err := rows.Scan(&l.ID, &prov, &kind, &l.Reference, &cur, &l.Gross, &l.Fee, &l.Net, &feeKnown, &txnAt, &settled,
			&from, &to, &l.ItemCount, &l.SourceLine); err != nil {
			return nil, err
		}
		l.Provider, l.Kind, l.Currency, l.FeeKnown = in1.get(prov), domain.LineKind(in1.get(kind)), domain.Currency(in1.get(cur)), feeKnown == 1
		l.TxnAt, l.CoversFrom, l.CoversTo = fromNull(txnAt), fromNull(from), fromNull(to)
		if err := l.SettledOn.UnmarshalText([]byte(settled)); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const (
	ledgerDetailsSQL = `SELECT id, provider, reference, amount, currency, created_at, from_account, to_account FROM ledger_txns WHERE id IN (`
	lineRawSQL       = `SELECT id, raw FROM settlement_lines WHERE id IN (`
)

// LedgerDetails returns full ledger rows (with accounts) for evidence.
func (s *Store) LedgerDetails(ctx context.Context, ids []string) (map[string]domain.LedgerTxn, error) {
	out := map[string]domain.LedgerTxn{}
	err := inChunks(ids, func(ph string, args []any) error {
		query := ledgerDetailsSQL + ph + ")" // ph holds only "?" placeholders
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var t domain.LedgerTxn
			var cur string
			var at int64
			if err := rows.Scan(&t.ID, &t.Provider, &t.Reference, &t.Amount, &cur, &at, &t.FromAccount, &t.ToAccount); err != nil {
				return err
			}
			t.Currency, t.CreatedAt = domain.Currency(cur), fromNanos(at)
			out[t.ID] = t
		}
		return rows.Err()
	})
	return out, err
}

// LineRaw returns source rows, verbatim, for evidence.
func (s *Store) LineRaw(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	err := inChunks(ids, func(ph string, args []any) error {
		query := lineRawSQL + ph + ")" // ph holds only "?" placeholders
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			out[id] = raw
		}
		return rows.Err()
	})
	return out, err
}

func inChunks(ids []string, fn func(placeholders string, args []any) error) error {
	const size = 500
	for start := 0; start < len(ids); start += size {
		chunk := ids[start:min(start+size, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if err := fn(strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ","), args); err != nil {
			return err
		}
	}
	return nil
}

// ---- runs ----

// Run is a stored, immutable reconciliation run.
type Run struct {
	ID            int64     `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	AsOf          time.Time `json:"as_of"`
	EngineVersion string    `json:"engine_version"`
	ConfigJSON    string    `json:"-"`
	ConfigSHA256  string    `json:"config_sha256"`
	MaxFileID     int64     `json:"max_file_id"`
	MaxEventSeq   int64     `json:"max_event_seq"`
	InputDigest   string    `json:"input_digest"`
	ResultDigest  string    `json:"result_digest"`
	LedgerCount   int       `json:"ledger_count"`
	LineCount     int       `json:"line_count"`
	PaymentCount  int       `json:"payment_count"`
	SummaryJSON   string    `json:"-"`
	DurationMS    int64     `json:"duration_ms"`
}

// ExceptionSync summarises how a run changed the exception queue.
type ExceptionSync struct {
	Opened     int `json:"opened"`
	Reopened   int `json:"reopened"`
	AutoClosed int `json:"auto_closed"`
	StillOpen  int `json:"still_open"`
	Resolved   int `json:"already_resolved"`
}

// SaveRun persists a run, its outcomes and members, and syncs the exception
// queue - one transaction. evidence returns JSON for discrepancy outcomes.
func (s *Store) SaveRun(ctx context.Context, r Run, res *match.Result, evidence func(i int) ([]byte, error)) (int64, ExceptionSync, error) {
	var runID int64
	var sync ExceptionSync
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		r.CreatedAt = now
		x, err := tx.ExecContext(ctx, `INSERT INTO runs (created_at, as_of, engine_version, config_json, config_sha256, max_file_id, max_event_seq,
			input_digest, result_digest, ledger_count, line_count, payment_count, summary_json, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			now.UnixNano(), r.AsOf.UnixNano(), r.EngineVersion, r.ConfigJSON, r.ConfigSHA256, r.MaxFileID, r.MaxEventSeq,
			r.InputDigest, r.ResultDigest, r.LedgerCount, r.LineCount, r.PaymentCount, r.SummaryJSON, r.DurationMS)
		if err != nil {
			return err
		}
		if runID, err = x.LastInsertId(); err != nil {
			return err
		}
		outs, err := newBatch(ctx, tx, `INSERT INTO run_outcomes (run_id, outcome_id, class, rule, provider, currency, expected_gross, expected_fee,
			expected_net, actual_gross, actual_fee, actual_net, deadline, settled_on, detail, evidence) VALUES `, 16, 32)
		if err != nil {
			return err
		}
		defer outs.close()
		mems, err := newBatch(ctx, tx, `INSERT INTO run_members (run_id, outcome_id, role, entity_id) VALUES `, 4, 128)
		if err != nil {
			return err
		}
		defer mems.close()
		for i := range res.Outcomes {
			o := &res.Outcomes[i]
			id := i + 1
			var ev any
			if o.Class.IsDiscrepancy() && evidence != nil {
				b, err := evidence(i)
				if err != nil {
					return err
				}
				ev = string(b)
			}
			if err := outs.add(runID, id, string(o.Class), string(o.Rule), o.Provider, string(o.Currency),
				o.Expected.Gross, o.Expected.Fee, o.Expected.Net, o.Actual.Gross, o.Actual.Fee, o.Actual.Net,
				o.Deadline.String(), o.SettledOn.String(), o.Detail, ev); err != nil {
				return err
			}
			for _, j := range o.Ledger {
				if err := mems.add(runID, id, "ledger", res.In.Ledger[j].ID); err != nil {
					return err
				}
			}
			for _, j := range o.Lines {
				if err := mems.add(runID, id, "line", res.In.Lines[j].ID); err != nil {
					return err
				}
			}
			for _, j := range o.Context {
				if err := mems.add(runID, id, "context", res.In.Lines[j].ID); err != nil {
					return err
				}
			}
		}
		if err := outs.flush(); err != nil {
			return err
		}
		if err := mems.flush(); err != nil {
			return err
		}
		sync, err = syncExceptions(ctx, tx, runID, now.UnixNano(), res)
		return err
	})
	return runID, sync, err
}

const runCols = `id, created_at, as_of, engine_version, config_json, config_sha256, max_file_id, max_event_seq, input_digest, result_digest,
	ledger_count, line_count, payment_count, summary_json, duration_ms`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var created, asOf int64
	err := sc.Scan(&r.ID, &created, &asOf, &r.EngineVersion, &r.ConfigJSON, &r.ConfigSHA256, &r.MaxFileID, &r.MaxEventSeq,
		&r.InputDigest, &r.ResultDigest, &r.LedgerCount, &r.LineCount, &r.PaymentCount, &r.SummaryJSON, &r.DurationMS)
	r.CreatedAt, r.AsOf = fromNanos(created), fromNanos(asOf)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// Run returns a run; id 0 means the latest.
func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	if id == 0 {
		return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT 1`))
	}
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

// Runs lists recent runs, newest first.
func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Item is one record's row in a run report: an outcome joined with one member.
type Item struct {
	OutcomeID int
	Class     domain.Class
	Rule      domain.Rule
	Provider  string
	Currency  domain.Currency
	Expected  match.Amounts
	Actual    match.Amounts
	Deadline  string
	SettledOn string
	Detail    string
	Role      string // ledger | line | context
	EntityID  string
	Reference string
	Amount    int64 // ledger amount, or line gross
}

// RunItems streams a run's report rows in outcome order.
func (s *Store) RunItems(ctx context.Context, runID int64, fn func(Item) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT o.outcome_id, o.class, o.rule, o.provider, o.currency,
		o.expected_gross, o.expected_fee, o.expected_net, o.actual_gross, o.actual_fee, o.actual_net, o.deadline, o.settled_on, o.detail,
		coalesce(m.role, ''), coalesce(m.entity_id, ''),
		coalesce(l.reference, sl.reference, ''), coalesce(l.amount, sl.gross, 0)
		FROM run_outcomes o
		LEFT JOIN run_members m ON m.run_id = o.run_id AND m.outcome_id = o.outcome_id
		LEFT JOIN ledger_txns l ON m.role = 'ledger' AND l.id = m.entity_id
		LEFT JOIN settlement_lines sl ON m.role <> 'ledger' AND sl.id = m.entity_id
		WHERE o.run_id = ? ORDER BY o.outcome_id, m.role, m.entity_id`, runID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var it Item
		var class, rule, cur string
		if err := rows.Scan(&it.OutcomeID, &class, &rule, &it.Provider, &cur,
			&it.Expected.Gross, &it.Expected.Fee, &it.Expected.Net, &it.Actual.Gross, &it.Actual.Fee, &it.Actual.Net,
			&it.Deadline, &it.SettledOn, &it.Detail, &it.Role, &it.EntityID, &it.Reference, &it.Amount); err != nil {
			return err
		}
		it.Class, it.Rule, it.Currency = domain.Class(class), domain.Rule(rule), domain.Currency(cur)
		if err := fn(it); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Evidence returns a stored outcome's evidence JSON.
func (s *Store) Evidence(ctx context.Context, runID int64, outcomeID int) (string, error) {
	var ev sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT evidence FROM run_outcomes WHERE run_id = ? AND outcome_id = ?`, runID, outcomeID).Scan(&ev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return ev.String, err
}

// ---- multi-row inserts ----

// batch accumulates rows and inserts them rowsPer at a time with one
// prepared multi-row INSERT; SQLite's per-statement overhead dominates
// single-row inserts at a million rows.
type batch struct {
	ctx     context.Context
	tx      *sql.Tx
	prefix  string
	cols    int
	rowsPer int
	full    *sql.Stmt
	args    []any
}

func placeholders(cols, rows int) string {
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(tuple+",", rows), ",")
}

func newBatch(ctx context.Context, tx *sql.Tx, prefix string, cols, rowsPer int) (*batch, error) {
	st, err := tx.PrepareContext(ctx, prefix+placeholders(cols, rowsPer))
	if err != nil {
		return nil, err
	}
	return &batch{ctx: ctx, tx: tx, prefix: prefix, cols: cols, rowsPer: rowsPer, full: st, args: make([]any, 0, cols*rowsPer)}, nil
}

func (b *batch) add(vals ...any) error {
	if len(vals) != b.cols {
		return fmt.Errorf("batch: %d values for %d columns", len(vals), b.cols)
	}
	b.args = append(b.args, vals...)
	if len(b.args) == b.cols*b.rowsPer {
		if _, err := b.full.ExecContext(b.ctx, b.args...); err != nil {
			return err
		}
		b.args = b.args[:0]
	}
	return nil
}

func (b *batch) flush() error {
	if len(b.args) == 0 {
		return nil
	}
	n := len(b.args) / b.cols
	_, err := b.tx.ExecContext(b.ctx, b.prefix+placeholders(b.cols, n), b.args...)
	b.args = b.args[:0]
	return err
}

func (b *batch) close() { _ = b.full.Close() }

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
