package sqlite

import (
	"context"
	"database/sql"

	"github.com/dwijamahardika/recond/internal/domain"
)

// EventResult is what one delivered webhook did.
type EventResult struct {
	Duplicate  bool           `json:"duplicate"`
	OutOfOrder bool           `json:"out_of_order"`
	Verdict    domain.Verdict `json:"verdict,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Before     domain.Status  `json:"status_before,omitempty"`
	After      domain.Status  `json:"status_after,omitempty"`
}

// ApplyEvent stores one webhook event and advances the payment's state, in
// one transaction. A re-delivered event id is a no-op reported as Duplicate.
func (s *Store) ApplyEvent(ctx context.Context, e domain.PaymentEvent, hash string) (EventResult, error) {
	var out EventResult
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := s.now().UnixNano()
		ok, err := insertEvent(ctx, tx, e, hash, now)
		if err != nil {
			return err
		}
		if !ok {
			out.Duplicate = true
			return nil
		}
		res, err := applyEvents(ctx, tx, []domain.PaymentEvent{e}, now, nil)
		if err != nil {
			return err
		}
		out = res[0]
		return nil
	})
	return out, err
}

// paymentCache carries payment states across the chunks of one file's
// ingestion, so the payments table is scanned at most once per file.
type paymentCache struct {
	m        map[domain.PaymentKey]domain.Payment
	complete bool // m holds every stored payment: a miss means a new payment
}

// applyEvents advances payment state for newly inserted events (arrival
// order). In-order events take the O(1) path; the first out-of-order event of
// a payment triggers a fold over its stored history, which already contains
// every event of this batch. cache may be nil.
func applyEvents(ctx context.Context, tx *sql.Tx, events []domain.PaymentEvent, now int64, cache *paymentCache) ([]EventResult, error) {
	var order []domain.PaymentKey
	groups := map[domain.PaymentKey][]int{}
	for i, e := range events {
		k := domain.PaymentKey{Provider: e.Provider, Ref: e.PaymentRef}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}

	if cache == nil {
		cache = &paymentCache{m: map[domain.PaymentKey]domain.Payment{}}
	}
	// Large batches: one sequential scan beats a point query per payment.
	if !cache.complete && len(order) > 2000 {
		all, err := scanPayments(ctx, tx, `SELECT `+paymentCols+` FROM payments`)
		if err != nil {
			return nil, err
		}
		for k, p := range cache.m { // states updated earlier in this transaction win
			all[k] = p
		}
		cache.m, cache.complete = all, true
	}
	get, err := tx.PrepareContext(ctx, `SELECT `+paymentCols+` FROM payments WHERE provider = ? AND payment_ref = ?`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = get.Close() }()
	put, err := tx.PrepareContext(ctx, `INSERT INTO payments (provider, payment_ref, status, lifecycle, amount, currency, refunded_amount, fully_refunded,
		paid_at, settled_at, last_seq, last_at, last_event_id, events, rejected, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, payment_ref) DO UPDATE SET status = excluded.status, lifecycle = excluded.lifecycle,
		amount = excluded.amount, currency = excluded.currency, refunded_amount = excluded.refunded_amount,
		fully_refunded = excluded.fully_refunded, paid_at = excluded.paid_at, settled_at = excluded.settled_at,
		last_seq = excluded.last_seq, last_at = excluded.last_at, last_event_id = excluded.last_event_id,
		events = excluded.events, rejected = excluded.rejected, updated_at = excluded.updated_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = put.Close() }()

	results := make([]EventResult, len(events))
	for _, k := range order {
		p, ok := cache.m[k]
		if !ok && !cache.complete {
			m, err := scanPaymentsStmt(ctx, get, k.Provider, k.Ref)
			if err != nil {
				return nil, err
			}
			p = m[k]
		}
		var before domain.Status
		if p.Events > 0 {
			before = p.Status()
		}
		existed := p.Events > 0
		idx := groups[k]
		// OutOfOrder: the delivery arrived after a newer event of the same
		// payment. Tracked per event so the count does not depend on how
		// events were batched.
		newest, seenAny := p.Last, p.Events > 0
		for _, i := range idx {
			v := events[i].Version()
			results[i].OutOfOrder = seenAny && v.Less(newest)
			if !seenAny || newest.Less(v) {
				newest, seenAny = v, true
			}
		}
		for n, i := range idx {
			v, reason, ok := p.Observe(events[i])
			if ok {
				results[i].Verdict, results[i].Reason = v, reason
				continue
			}
			// Out of order: fold the payment's full history. For a payment
			// first seen in this batch, that history is exactly the batch's
			// events for it - no query needed.
			var hist []domain.PaymentEvent
			if existed {
				var err error
				if hist, err = history(ctx, tx, k, 0); err != nil {
					return nil, err
				}
			} else {
				hist = make([]domain.PaymentEvent, 0, len(idx))
				for _, j := range idx {
					hist = append(hist, events[j])
				}
			}
			var rej []domain.Rejection
			p, rej = domain.Fold(hist)
			for _, j := range idx[n:] {
				results[j].Verdict, results[j].Reason = domain.Applied, ""
				for _, r := range rej {
					if r.EventID == events[j].EventID {
						results[j].Verdict, results[j].Reason = domain.Rejected, r.Reason
					}
				}
			}
			break
		}
		for _, i := range idx {
			results[i].Before, results[i].After = before, p.Status()
		}
		cache.m[k] = p
		if _, err := put.ExecContext(ctx, p.Provider, p.Ref, string(p.Status()), string(p.Lifecycle), p.Amount, string(p.Currency),
			p.RefundedAmount, boolInt(p.FullyRefunded), nullTime(p.PaidAt), nullTime(p.SettledAt),
			p.Last.Seq, p.Last.At, p.Last.ID, p.Events, p.Rejected, now); err != nil {
			return nil, err
		}
	}
	return results, nil
}

const eventCols = `provider, event_id, payment_ref, status, amount, refunded_amount, currency, provider_seq, occurred_at`

func scanEvent(rows *sql.Rows) (domain.PaymentEvent, error) {
	var e domain.PaymentEvent
	var status, cur string
	var at int64
	err := rows.Scan(&e.Provider, &e.EventID, &e.PaymentRef, &status, &e.Amount, &e.RefundedAmount, &cur, &e.Sequence, &at)
	e.Status, e.Currency, e.OccurredAt = domain.Status(status), domain.Currency(cur), fromNanos(at)
	return e, err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// history returns a payment's events in arrival order (maxSeq 0 = all).
func history(ctx context.Context, q querier, k domain.PaymentKey, maxSeq int64) ([]domain.PaymentEvent, error) {
	query := `SELECT ` + eventCols + ` FROM webhook_events WHERE provider = ? AND payment_ref = ? ORDER BY seq`
	args := []any{k.Provider, k.Ref}
	if maxSeq > 0 {
		query = `SELECT ` + eventCols + ` FROM webhook_events WHERE provider = ? AND payment_ref = ? AND seq <= ? ORDER BY seq`
		args = append(args, maxSeq)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.PaymentEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const paymentCols = `provider, payment_ref, lifecycle, amount, currency, refunded_amount, fully_refunded, paid_at, settled_at, last_seq, last_at, last_event_id, events, rejected`

func scanPaymentRows(rows *sql.Rows, out map[domain.PaymentKey]domain.Payment) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p domain.Payment
		var lifecycle, cur string
		var full int
		var paid, settled sql.NullInt64
		if err := rows.Scan(&p.Provider, &p.Ref, &lifecycle, &p.Amount, &cur, &p.RefundedAmount, &full, &paid, &settled,
			&p.Last.Seq, &p.Last.At, &p.Last.ID, &p.Events, &p.Rejected); err != nil {
			return err
		}
		p.Lifecycle, p.Currency, p.FullyRefunded = domain.Status(lifecycle), domain.Currency(cur), full == 1
		p.PaidAt, p.SettledAt = fromNull(paid), fromNull(settled)
		out[domain.PaymentKey{Provider: p.Provider, Ref: p.Ref}] = p
	}
	return rows.Err()
}

func scanPayments(ctx context.Context, q querier, query string, args ...any) (map[domain.PaymentKey]domain.Payment, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := map[domain.PaymentKey]domain.Payment{}
	return out, scanPaymentRows(rows, out)
}

func scanPaymentsStmt(ctx context.Context, st *sql.Stmt, args ...any) (map[domain.PaymentKey]domain.Payment, error) {
	rows, err := st.QueryContext(ctx, args...)
	if err != nil {
		return nil, err
	}
	out := map[domain.PaymentKey]domain.Payment{}
	return out, scanPaymentRows(rows, out)
}

// Payment returns one payment's current state and its events in arrival order.
func (s *Store) Payment(ctx context.Context, provider, ref string) (domain.Payment, []domain.PaymentEvent, error) {
	k := domain.PaymentKey{Provider: provider, Ref: ref}
	m, err := scanPayments(ctx, s.db, `SELECT `+paymentCols+` FROM payments WHERE provider = ? AND payment_ref = ?`, provider, ref)
	if err != nil {
		return domain.Payment{}, nil, err
	}
	p, ok := m[k]
	if !ok {
		return domain.Payment{}, nil, ErrNotFound
	}
	h, err := history(ctx, s.db, k, 0)
	return p, h, err
}

// foldPaymentsAt rebuilds every payment's state from the event log up to
// maxSeq. Replays use this instead of the payments projection, which only
// reflects the latest events - and so double as a consistency check of it.
func foldPaymentsAt(ctx context.Context, q querier, maxSeq int64) (map[domain.PaymentKey]domain.Payment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+eventCols+` FROM webhook_events WHERE seq <= ? ORDER BY provider, payment_ref, seq`, maxSeq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[domain.PaymentKey]domain.Payment{}
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
			return nil, err
		}
		k := domain.PaymentKey{Provider: e.Provider, Ref: e.PaymentRef}
		if k != cur {
			flush()
			cur = k
		}
		batch = append(batch, e)
	}
	flush()
	return out, rows.Err()
}
