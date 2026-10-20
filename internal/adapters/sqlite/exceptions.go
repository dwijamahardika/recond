package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/match"
)

// Exception statuses.
const (
	StatusOpen       = "open"
	StatusResolved   = "resolved"
	StatusAutoClosed = "auto_closed" // the discrepancy disappeared in a later run
)

// Exception actions recorded in the audit trail.
const (
	ActionOpened     = "opened"
	ActionResolved   = "resolved"
	ActionReopened   = "reopened"
	ActionAutoClosed = "auto_closed"
)

// SystemActor is the actor recorded for changes recond makes itself.
const SystemActor = "recond"

// Exception is one discrepancy in the queue (a projection of its events).
type Exception struct {
	ID          int64           `json:"id"`
	Fingerprint string          `json:"fingerprint"`
	Class       domain.Class    `json:"class"`
	Provider    string          `json:"provider"`
	Currency    domain.Currency `json:"currency"`
	Status      string          `json:"status"`
	FirstRun    int64           `json:"first_run"`
	LastRun     int64           `json:"last_run"`
	RunID       int64           `json:"run_id"`
	OutcomeID   int             `json:"outcome_id"`
	Refs        string          `json:"refs"`
	Delta       int64           `json:"delta"`
	Detail      string          `json:"detail"`
	OpenedAt    int64           `json:"-"`
	UpdatedAt   int64           `json:"-"`
}

// ExceptionEvent is one append-only audit trail entry.
type ExceptionEvent struct {
	ID     int64  `json:"id"`
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
	RunID  *int64 `json:"run_id,omitempty"`
	At     int64  `json:"at"`
}

// fingerprint identifies a discrepancy across runs: its class and the exact
// records involved. A changed amount on the same records is the same
// exception (evidence updates); a different set of records is a new one.
func fingerprint(o *match.Outcome, res *match.Result) (string, string) {
	ids := make([]string, 0, len(o.Ledger)+len(o.Lines))
	refs := make([]string, 0, len(o.Ledger)+len(o.Lines))
	for _, i := range o.Ledger {
		ids = append(ids, "L:"+res.In.Ledger[i].ID)
		refs = append(refs, res.In.Ledger[i].Reference)
	}
	for _, i := range o.Lines {
		ids = append(ids, "S:"+res.In.Lines[i].ID)
		if len(o.Ledger) == 0 {
			refs = append(refs, res.In.Lines[i].Reference)
		}
	}
	sort.Strings(ids)
	h := sha256.New()
	h.Write([]byte(o.Class))
	for _, id := range ids {
		h.Write([]byte{0})
		h.Write([]byte(id))
	}
	summary := strings.Join(refs[:min(len(refs), 3)], ", ")
	if len(refs) > 3 {
		summary += fmt.Sprintf(" +%d more", len(refs)-3)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), summary
}

func syncExceptions(ctx context.Context, tx *sql.Tx, runID, now int64, res *match.Result) (ExceptionSync, error) {
	var sync ExceptionSync
	type known struct {
		id     int64
		status string
	}
	existing := map[string]known{}
	err := func() error {
		rows, err := tx.QueryContext(ctx, `SELECT id, fingerprint, status FROM exceptions`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k known
			var fp string
			if err := rows.Scan(&k.id, &fp, &k.status); err != nil {
				return err
			}
			existing[fp] = k
		}
		return rows.Err()
	}()
	if err != nil {
		return sync, err
	}

	seen := map[string]bool{}
	for i := range res.Outcomes {
		o := &res.Outcomes[i]
		if !o.Class.IsDiscrepancy() {
			continue
		}
		fp, refs := fingerprint(o, res)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		delta := o.Actual.Net - o.Expected.Net
		k, ok := existing[fp]
		if !ok {
			x, err := tx.ExecContext(ctx, `INSERT INTO exceptions (fingerprint, class, provider, currency, status, first_run, last_run, run_id, outcome_id,
				refs, delta, detail, opened_at, updated_at) VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				fp, string(o.Class), o.Provider, string(o.Currency), runID, runID, runID, i+1, refs, delta, o.Detail, now, now)
			if err != nil {
				return sync, err
			}
			id, err := x.LastInsertId()
			if err != nil {
				return sync, err
			}
			if err := appendEvent(ctx, tx, id, ActionOpened, SystemActor, "found by run "+itoa(runID), &runID, now); err != nil {
				return sync, err
			}
			sync.Opened++
			continue
		}
		status := k.status
		switch k.status {
		case StatusAutoClosed:
			status = StatusOpen
			if err := appendEvent(ctx, tx, k.id, ActionReopened, SystemActor, "reappeared in run "+itoa(runID), &runID, now); err != nil {
				return sync, err
			}
			sync.Reopened++
		case StatusResolved:
			sync.Resolved++ // a human decision stands until a human reopens it
		default:
			sync.StillOpen++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE exceptions SET status = ?, last_run = ?, run_id = ?, outcome_id = ?, refs = ?, delta = ?, detail = ?, updated_at = ?
			WHERE id = ?`, status, runID, runID, i+1, refs, delta, o.Detail, now, k.id); err != nil {
			return sync, err
		}
	}
	stale := make([]string, 0)
	for fp, k := range existing {
		if !seen[fp] && k.status == StatusOpen {
			stale = append(stale, fp)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return existing[stale[i]].id < existing[stale[j]].id })
	for _, fp := range stale {
		k := existing[fp]
		if _, err := tx.ExecContext(ctx, `UPDATE exceptions SET status = 'auto_closed', updated_at = ? WHERE id = ?`, now, k.id); err != nil {
			return sync, err
		}
		if err := appendEvent(ctx, tx, k.id, ActionAutoClosed, SystemActor, "not present in run "+itoa(runID), &runID, now); err != nil {
			return sync, err
		}
		sync.AutoClosed++
	}
	return sync, nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, id int64, action, actor, reason string, runID *int64, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO exception_events (exception_id, action, actor, reason, run_id, at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, action, actor, reason, runID, at)
	return err
}

// ExceptionFilter narrows a listing.
type ExceptionFilter struct {
	Status   string
	Class    string
	Provider string
	AfterID  int64
	Limit    int
}

const exceptionCols = `id, fingerprint, class, provider, currency, status, first_run, last_run, run_id, outcome_id, refs, delta, detail, opened_at, updated_at`

func scanException(sc interface{ Scan(...any) error }) (Exception, error) {
	var e Exception
	var class, cur string
	err := sc.Scan(&e.ID, &e.Fingerprint, &class, &e.Provider, &cur, &e.Status, &e.FirstRun, &e.LastRun, &e.RunID, &e.OutcomeID,
		&e.Refs, &e.Delta, &e.Detail, &e.OpenedAt, &e.UpdatedAt)
	e.Class, e.Currency = domain.Class(class), domain.Currency(cur)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}

// Exceptions lists exceptions by id (keyset pagination via AfterID).
func (s *Store) Exceptions(ctx context.Context, f ExceptionFilter) ([]Exception, error) {
	q := `SELECT ` + exceptionCols + ` FROM exceptions WHERE id > ?`
	args := []any{f.AfterID}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Class != "" {
		q += ` AND class = ?`
		args = append(args, f.Class)
	}
	if f.Provider != "" {
		q += ` AND provider = ?`
		args = append(args, f.Provider)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Exception
	for rows.Next() {
		e, err := scanException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Exception returns one exception and its full audit trail, oldest first.
func (s *Store) Exception(ctx context.Context, id int64) (Exception, []ExceptionEvent, error) {
	e, err := scanException(s.db.QueryRowContext(ctx, `SELECT `+exceptionCols+` FROM exceptions WHERE id = ?`, id))
	if err != nil {
		return e, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, action, actor, reason, run_id, at FROM exception_events WHERE exception_id = ? ORDER BY id`, id)
	if err != nil {
		return e, nil, err
	}
	defer func() { _ = rows.Close() }()
	var evs []ExceptionEvent
	for rows.Next() {
		var ev ExceptionEvent
		var run sql.NullInt64
		if err := rows.Scan(&ev.ID, &ev.Action, &ev.Actor, &ev.Reason, &run, &ev.At); err != nil {
			return e, nil, err
		}
		if run.Valid {
			ev.RunID = &run.Int64
		}
		evs = append(evs, ev)
	}
	return e, evs, rows.Err()
}

// Transition applies a human decision: resolve an open exception, or reopen
// a resolved/auto-closed one. The event is appended and the status projection
// updated in one transaction; an invalid transition is ErrConflict.
func (s *Store) Transition(ctx context.Context, id int64, action, actor, reason string) (Exception, error) {
	var out Exception
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		e, err := scanException(tx.QueryRowContext(ctx, `SELECT `+exceptionCols+` FROM exceptions WHERE id = ?`, id))
		if err != nil {
			return err
		}
		var next string
		switch {
		case action == ActionResolved && e.Status == StatusOpen:
			next = StatusResolved
		case action == ActionReopened && (e.Status == StatusResolved || e.Status == StatusAutoClosed):
			next = StatusOpen
		default:
			verb := map[string]string{ActionResolved: "resolve", ActionReopened: "reopen"}[action]
			if verb == "" {
				verb = action
			}
			return fmt.Errorf("%w: cannot %s an exception that is %s", ErrConflict, verb, e.Status)
		}
		now := s.now().UnixNano()
		if err := appendEvent(ctx, tx, id, action, actor, reason, nil, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE exceptions SET status = ?, updated_at = ? WHERE id = ?`, next, now, id); err != nil {
			return err
		}
		e.Status, e.UpdatedAt = next, now
		out = e
		return nil
	})
	return out, err
}

// ExceptionCounts returns counts by status.
func (s *Store) ExceptionCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM exceptions GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{StatusOpen: 0, StatusResolved: 0, StatusAutoClosed: 0}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
