package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// Statements exactly as the previous release (schema 0001) wrote them. They
// are frozen here on purpose: the current code writes more columns, and the
// point of the test is a database the old binary left behind.
const (
	prevInsertRun = `INSERT INTO runs (created_at, as_of, engine_version, config_json, config_sha256, max_file_id, max_event_seq,
		input_digest, result_digest, ledger_count, line_count, payment_count, summary_json, duration_ms)
		VALUES (1, 2, '1', '{}', 'cfg', 1, 0, 'in', 'out', 1, 0, 0, '{}', 5)`
	prevInsertException = `INSERT INTO exceptions (fingerprint, class, provider, currency, status, first_run, last_run, run_id, outcome_id,
		refs, delta, detail, opened_at, updated_at) VALUES ('fp', 'missing_in_settlement', 'psp_a', 'IDR', 'open', 1, 1, 1, 1, 'ORD-t1', 0, '', 1, 1)`
)

// A database written by the previous release opens with this one: 0002
// applies on open, the old run reads back intact with no window, the old
// exception is undated (so a windowed run will never auto-close it), and the
// window indexes exist.
func TestMigrationFromSchema0001(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recond.db")
	old, err := openUpTo(ctx, path, "0001_init")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Ingest(ctx, FileMeta{Kind: "ledger", Source: "ledger", Name: "l.csv", SHA256: "f1"}, func(in *Ingester) error {
		return in.Ledger(ledgerRow("t1", 100), "h")
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{prevInsertRun, prevInsertException} {
		if _, err := old.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("previous release statement: %v", err)
		}
	}
	var cols int
	if err := old.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('runs') WHERE name LIKE 'window_%'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("schema 0001 must not have window columns: %d %v", cols, err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open with this release: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r, err := s.Run(ctx, 1)
	if err != nil || r.ResultDigest != "out" || r.LedgerCount != 1 || r.DurationMS != 5 {
		t.Fatalf("pre-existing run: %+v %v", r, err)
	}
	var from, date any
	if err := s.db.QueryRowContext(ctx, `SELECT window_from FROM runs WHERE id = 1`).Scan(&from); err != nil || from != nil {
		t.Fatalf("pre-existing run must read as full history (NULL window): %v %v", from, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT window_date FROM exceptions WHERE id = 1`).Scan(&date); err != nil || date != nil {
		t.Fatalf("pre-existing exception must be undated: %v %v", date, err)
	}
	for _, idx := range []string{"ledger_txns_created", "ledger_txns_ref", "settlement_lines_window", "settlement_lines_untimed",
		"settlement_lines_item_ref", "settlement_lines_payout"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing after upgrade (%v)", idx, err)
		}
	}
	st, err := s.SchemaStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 2 || st.Applied[0].Version != "0001_init" || st.Applied[1].Version != "0002_windowed_runs" {
		t.Fatalf("applied %+v", st.Applied)
	}
	if st.UndatedByState[StatusOpen] != 1 {
		t.Fatalf("undated open exceptions %+v", st.UndatedByState)
	}
	// The append-only guarantees survive the migration.
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET window_from = '2026-10-01'`); err == nil {
		t.Fatal("runs must stay immutable after the migration")
	}
}
