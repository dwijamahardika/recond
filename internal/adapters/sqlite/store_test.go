package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/ingest"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recond.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

var t0 = time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)

func ledgerRow(id string, amount int64) domain.LedgerTxn {
	return domain.LedgerTxn{ID: id, Provider: "psp_a", Reference: "ORD-" + id, Amount: amount, Currency: "IDR", CreatedAt: t0, FromAccount: "a", ToAccount: "b"}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	_, path := open(t)
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}

func TestIngestIdempotency(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	add := func(sha string, rows ...domain.LedgerTxn) (IngestStats, error) {
		return s.Ingest(ctx, FileMeta{Kind: "ledger", Source: "ledger", Name: "l.csv", SHA256: sha}, func(in *Ingester) error {
			for _, r := range rows {
				if err := in.Ledger(r, ingest.RowHash(r.ID, r.Reference, string(rune(r.Amount)))); err != nil {
					return err
				}
			}
			return nil
		})
	}
	st, err := add("f1", ledgerRow("t1", 100), ledgerRow("t2", 200))
	if err != nil || st.Inserted != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	// Same content hash: the whole file is skipped.
	if _, err := add("f1", ledgerRow("t1", 100)); !errors.Is(err, ErrAlreadyIngested) {
		t.Fatalf("want ErrAlreadyIngested, got %v", err)
	}
	// Different file overlapping on t2 (same content) and t1 (changed amount).
	st, err = add("f2", ledgerRow("t2", 200), ledgerRow("t1", 999), ledgerRow("t3", 300))
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 1 || st.Duplicates != 1 || st.Conflicts != 1 || st.Rejected != 1 {
		t.Fatalf("overlap stats %+v", st)
	}
	in, err := s.LoadInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range in.Ledger {
		if l.ID == "t1" && l.Amount != 100 {
			t.Fatal("first version must be kept on conflict")
		}
	}
	// A failing file leaves nothing behind.
	_, err = s.Ingest(ctx, FileMeta{Kind: "ledger", Source: "ledger", Name: "bad.csv", SHA256: "f3"}, func(in *Ingester) error {
		_ = in.Ledger(ledgerRow("t9", 1), "h")
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if id, ok, _ := s.FileBySHA(ctx, "f3"); ok {
		t.Fatalf("failed file recorded as %d", id)
	}
}

func TestAppendOnlyTriggers(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.Ingest(ctx, FileMeta{Kind: "ledger", Source: "ledger", Name: "l", SHA256: "x"}, func(in *Ingester) error {
		return in.Ledger(ledgerRow("t1", 100), "h")
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO runs (created_at, as_of, engine_version, config_json, config_sha256, max_file_id, max_event_seq, input_digest,
			result_digest, ledger_count, line_count, payment_count, summary_json, duration_ms) VALUES (0, 0, '1', '{}', '', 0, 0, '', '', 0, 0, 0, '{}', 0)`,
		`INSERT INTO exceptions (fingerprint, class, provider, currency, status, first_run, last_run, run_id, outcome_id, refs, delta, detail, opened_at, updated_at)
			VALUES ('fp', 'amount_mismatch', 'psp_a', 'IDR', 'open', 1, 1, 1, 1, '', 0, '', 0, 0)`,
		`INSERT INTO exception_events (exception_id, action, actor, reason, at) VALUES (1, 'opened', 'recond', 'r', 0)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE ledger_txns SET amount = 1`,
		`DELETE FROM ledger_txns`,
		`UPDATE exception_events SET actor = 'someone else'`,
		`DELETE FROM exception_events`,
		`UPDATE runs SET result_digest = 'forged'`,
		`DELETE FROM runs`,
	} {
		_, err := s.db.ExecContext(ctx, q)
		if err == nil || (!strings.Contains(err.Error(), "append-only") && !strings.Contains(err.Error(), "immutable")) {
			t.Errorf("%s: want trigger error, got %v", q, err)
		}
	}
}

func evt(id string, seq int64, st domain.Status) domain.PaymentEvent {
	return domain.PaymentEvent{EventID: id, Provider: "psp_a", PaymentRef: "ORD-1", Status: st, Amount: 1000, Currency: "IDR",
		Sequence: seq, OccurredAt: t0.Add(time.Duration(seq) * time.Second)}
}

func TestApplyEvent(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	steps := []struct {
		e    domain.PaymentEvent
		want EventResult
	}{
		{evt("e2", 2, domain.StatusPaid), EventResult{Verdict: domain.Applied, After: domain.StatusPaid}},
		{evt("e2", 2, domain.StatusPaid), EventResult{Duplicate: true}},
		{evt("e1", 1, domain.StatusPending), EventResult{OutOfOrder: true, Verdict: domain.Applied, Before: domain.StatusPaid, After: domain.StatusPaid}},
		{evt("e3", 3, domain.StatusSettled), EventResult{Verdict: domain.Applied, Before: domain.StatusPaid, After: domain.StatusSettled}},
	}
	for i, st := range steps {
		got, err := s.ApplyEvent(ctx, st.e, ingest.EventHash(st.e))
		if err != nil {
			t.Fatal(err)
		}
		if got != st.want {
			t.Fatalf("step %d: got %+v want %+v", i, got, st.want)
		}
	}
	p, hist, err := s.Payment(ctx, "psp_a", "ORD-1")
	if err != nil || p.Status() != domain.StatusSettled || len(hist) != 3 {
		t.Fatalf("%+v %d %v", p, len(hist), err)
	}
	// The projection equals a fold of the log.
	cur, err := s.LoadInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	past, err := s.LoadInputsAt(ctx, cur.MaxFileID, cur.MaxEventSeq)
	if err != nil {
		t.Fatal(err)
	}
	k := domain.PaymentKey{Provider: "psp_a", Ref: "ORD-1"}
	if cur.Payments[k] != past.Payments[k] {
		t.Fatalf("projection %+v != fold %+v", cur.Payments[k], past.Payments[k])
	}
	// As of the first two events only, the payment was merely paid.
	early, err := s.LoadInputsAt(ctx, 0, 2)
	if err != nil || early.Payments[k].Status() != domain.StatusPaid {
		t.Fatalf("watermarked fold: %+v %v", early.Payments[k], err)
	}
}

func TestBatchInsertSizes(t *testing.T) {
	if got := placeholders(2, 3); got != "(?,?),(?,?),(?,?)" {
		t.Fatal(got)
	}
}
