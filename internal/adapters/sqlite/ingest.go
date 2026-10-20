package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/ingest"
)

// FileMeta describes one input file.
type FileMeta struct {
	Kind   string // ledger | settlement | webhooks
	Source string // provider, or "ledger"
	Name   string
	SHA256 string
	Size   int64
}

// IngestStats summarises one file's ingestion.
type IngestStats struct {
	FileID     int64 `json:"file_id"`
	Rows       int   `json:"rows"`
	Inserted   int   `json:"inserted"`
	Duplicates int   `json:"duplicates"` // same key, same content: harmless re-delivery
	Conflicts  int   `json:"conflicts"`  // same key, different content: first version kept, row rejected
	Rejected   int   `json:"rejected"`   // includes conflicts
	// Webhook files only.
	OutOfOrder          int `json:"out_of_order,omitempty"`
	RejectedTransitions int `json:"rejected_transitions,omitempty"`
}

// FileBySHA returns the id of an already-ingested file with this content hash.
func (s *Store) FileBySHA(ctx context.Context, sha string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM ingest_files WHERE sha256 = ?`, sha).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// Ingester adds one file's rows inside a single transaction, one prepared
// single-row INSERT ... ON CONFLICT DO NOTHING per row. (Multi-row INSERT ...
// RETURNING was measured and was slower with this driver; see the README.)
type Ingester struct {
	ctx    context.Context
	tx     *sql.Tx
	fileID int64
	now    int64
	stats  IngestStats
	events []domain.PaymentEvent // newly inserted, arrival order
	// seen remembers event keys inserted by this file and their content hash:
	// webhook streams repeat deliveries within one file (at-least-once), and a
	// repeat is then classified without a database lookup. Ledger and
	// settlement files rarely repeat keys, so they skip the memory cost.
	seen map[string]string
	// pay carries payment states across event chunks (see applyChunk).
	pay *paymentCache

	ledgerStmt, lineStmt, eventStmt, rejectStmt *sql.Stmt
}

const (
	insertLedger = `INSERT INTO ledger_txns (id, file_id, provider, reference, amount, currency, created_at, from_account, to_account, row_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`
	insertLine = `INSERT INTO settlement_lines (id, file_id, provider, kind, reference, currency, gross, fee, net, fee_known,
		txn_at, settled_on, covers_from, covers_to, item_count, source_line, raw, row_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`
	insertEventSQL = `INSERT INTO webhook_events (provider, event_id, payment_ref, status, amount, refunded_amount, currency, provider_seq, occurred_at, received_at, file_id, row_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (provider, event_id) DO NOTHING`
	insertReject = `INSERT INTO ingest_rejects (file_id, line, reason, raw) VALUES (?, ?, ?, ?)`
)

// Ingest records a new file and runs fn to add its rows; the file and all its
// rows commit together or not at all. Content already ingested (same SHA-256)
// returns ErrAlreadyIngested and the existing file's id - re-running an
// import is always safe.
func (s *Store) Ingest(ctx context.Context, meta FileMeta, fn func(*Ingester) error) (IngestStats, error) {
	var stats IngestStats
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM ingest_files WHERE sha256 = ?`, meta.SHA256).Scan(&existing)
		if err == nil {
			stats.FileID = existing
			return ErrAlreadyIngested
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := s.now().UnixNano()
		res, err := tx.ExecContext(ctx, `INSERT INTO ingest_files (kind, source, name, sha256, size_bytes, ingested_at) VALUES (?, ?, ?, ?, ?, ?)`,
			meta.Kind, meta.Source, meta.Name, meta.SHA256, meta.Size, now)
		if err != nil {
			return err
		}
		in := &Ingester{ctx: ctx, tx: tx, now: now, seen: map[string]string{}, pay: &paymentCache{m: map[domain.PaymentKey]domain.Payment{}}}
		if in.fileID, err = res.LastInsertId(); err != nil {
			return err
		}
		in.stats.FileID = in.fileID
		if in.ledgerStmt, err = tx.PrepareContext(ctx, insertLedger); err != nil {
			return err
		}
		defer func() { _ = in.ledgerStmt.Close() }()
		if in.lineStmt, err = tx.PrepareContext(ctx, insertLine); err != nil {
			return err
		}
		defer func() { _ = in.lineStmt.Close() }()
		if in.eventStmt, err = tx.PrepareContext(ctx, insertEventSQL); err != nil {
			return err
		}
		defer func() { _ = in.eventStmt.Close() }()
		if in.rejectStmt, err = tx.PrepareContext(ctx, insertReject); err != nil {
			return err
		}
		defer func() { _ = in.rejectStmt.Close() }()

		if err := fn(in); err != nil {
			return err
		}
		if err := in.applyChunk(); err != nil {
			return err
		}
		st := in.stats
		if _, err := tx.ExecContext(ctx, `UPDATE ingest_files SET rows_total = ?, rows_inserted = ?, rows_duplicate = ?, rows_conflict = ?, rows_rejected = ? WHERE id = ?`,
			st.Rows, st.Inserted, st.Duplicates, st.Conflicts, st.Rejected, in.fileID); err != nil {
			return err
		}
		stats = st
		return nil
	})
	return stats, err
}

// eventChunk bounds how many new events are held before their payment
// states are advanced. All of it is one transaction either way; chunking
// only caps memory on multi-million-event backfills.
var eventChunk = 100_000 // a var so tests can force many chunks

func (in *Ingester) applyChunk() error {
	if len(in.events) == 0 {
		return nil
	}
	results, err := applyEvents(in.ctx, in.tx, in.events, in.now, in.pay)
	if err != nil {
		return fmt.Errorf("apply events: %w", err)
	}
	for _, r := range results {
		if r.OutOfOrder {
			in.stats.OutOfOrder++
		}
		if r.Verdict == domain.Rejected {
			in.stats.RejectedTransitions++
		}
	}
	in.events = in.events[:0]
	return nil
}

// row inserts one record unless this file already carried its key; a key
// that hits an existing row is classified as a duplicate (same content) or a
// conflict (different content: the first version is kept, the row rejected).
func (in *Ingester) row(st *sql.Stmt, lookup, key, hash string, line int, raw string, args ...any) (bool, error) {
	in.stats.Rows++
	remember := lookup == lookupEvent
	if prev, ok := in.seen[key]; remember && ok {
		return false, in.classify(prev, hash, key, line, raw)
	}
	res, err := st.ExecContext(in.ctx, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		in.stats.Inserted++
		if remember {
			in.seen[key] = hash
		}
		return true, nil
	}
	var stored string
	lookupArgs := []any{key}
	if lookup == lookupEvent {
		lookupArgs = []any{args[0], args[1]}
	}
	if err := in.tx.QueryRowContext(in.ctx, lookup, lookupArgs...).Scan(&stored); err != nil {
		return false, fmt.Errorf("lookup existing %s: %w", key, err)
	}
	return false, in.classify(stored, hash, key, line, raw)
}

const (
	lookupLedger = `SELECT row_hash FROM ledger_txns WHERE id = ?`
	lookupLine   = `SELECT row_hash FROM settlement_lines WHERE id = ?`
	lookupEvent  = `SELECT row_hash FROM webhook_events WHERE provider = ? AND event_id = ?`
)

func (in *Ingester) classify(stored, hash, key string, line int, raw string) error {
	if stored == hash {
		in.stats.Duplicates++
		return nil
	}
	in.stats.Conflicts++
	return in.Reject(ingest.Reject{Line: line, Reason: "conflict: " + key + " already ingested with different content; first version kept", Raw: raw})
}

// Ledger adds one ledger transaction.
func (in *Ingester) Ledger(t domain.LedgerTxn, hash string) error {
	_, err := in.row(in.ledgerStmt, lookupLedger, t.ID, hash, 0, t.ID,
		t.ID, in.fileID, t.Provider, t.Reference, t.Amount, string(t.Currency), t.CreatedAt.UnixNano(), t.FromAccount, t.ToAccount, hash)
	return err
}

// Line adds one settlement line.
func (in *Ingester) Line(l domain.SettlementLine, hash string) error {
	_, err := in.row(in.lineStmt, lookupLine, l.ID, hash, l.SourceLine, l.Raw,
		l.ID, in.fileID, l.Provider, string(l.Kind), l.Reference, string(l.Currency), l.Gross, l.Fee, l.Net,
		boolInt(l.FeeKnown), nullTime(l.TxnAt), l.SettledOn.String(), nullTime(l.CoversFrom), nullTime(l.CoversTo), l.ItemCount, l.SourceLine, l.Raw, hash)
	return err
}

// Event adds one webhook event; payment state is updated at commit, in
// arrival order.
func (in *Ingester) Event(e domain.PaymentEvent, hash string) error {
	key := e.Provider + "/" + e.EventID
	ok, err := in.row(in.eventStmt, lookupEvent, key, hash, 0, e.EventID,
		eventArgs(e, hash, sql.NullInt64{Int64: in.fileID, Valid: true}, in.now)...)
	if err != nil || !ok {
		return err
	}
	in.events = append(in.events, e)
	if len(in.events) >= eventChunk {
		return in.applyChunk()
	}
	return nil
}

func eventArgs(e domain.PaymentEvent, hash string, fileID sql.NullInt64, now int64) []any {
	return []any{e.Provider, e.EventID, e.PaymentRef, string(e.Status), e.Amount, e.RefundedAmount, string(e.Currency), e.Sequence,
		e.OccurredAt.UnixNano(), now, fileID, hash}
}

// Reject records a row that could not be ingested.
func (in *Ingester) Reject(r ingest.Reject) error {
	in.stats.Rejected++
	_, err := in.rejectStmt.ExecContext(in.ctx, in.fileID, r.Line, r.Reason, r.Raw)
	return err
}

// insertEvent inserts one event outside file ingestion (the webhook path) and
// reports whether it was new.
func insertEvent(ctx context.Context, tx *sql.Tx, e domain.PaymentEvent, hash string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx, insertEventSQL, eventArgs(e, hash, sql.NullInt64{}, now)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
