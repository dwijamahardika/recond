# ADR 0001: Boundary semantics for windowed reconciliation runs

- **Status:** Accepted
- **Date:** 2026-11-05
- **Context documents:** [PRD](../lifecycle/windowed-runs/01-prd.md), [RFC](../lifecycle/windowed-runs/02-rfc.md)

## Context

recond is gaining date-windowed runs: reconcile only business dates `[from, to]` instead of all history. Records that belong together do not share a timestamp. The ledger books after the provider (milliseconds, sometimes minutes), settlements arrive T+N days later, and payouts cover multi-hour cycles. Any window boundary therefore cuts through some groups. Three decisions determine whether consecutive windows add up to the same answer as one full run: which date puts a record in a window, which run reports a group that spans a boundary, and what happens when a run cannot see a counterpart. They interact, and getting any of them wrong shows up as double-counted or lost discrepancies in a finance close. So they are recorded together.

Constraints: inputs are append-only (no backfilling stored rows); the engine stays a pure function of its input; unwindowed runs must not change; storage must be able to select a window's records with an index, or the feature saves nothing.

## Decision

1. **Window dates.** Each record has one window date, a civil date in *its provider's* configured timezone (the T+N calendar). For ledger transactions it is the booking date (`created_at`). For itemised settlement lines it is the provider's transaction date (`txn_at`), or `settled_on − settlement_days` when the provider gives no transaction time. For payouts it is the start of the covered period. Matching is unchanged and still uses the provider's clock.
2. **Visible range.** A run with window `[from, to]` and margins `lookbehind`, `lookahead` (whole days, 1–31, default 1) matches every record dated in `[from − lookbehind, to + lookahead]`, and only those.
3. **Ownership: earliest record wins.** A run reports a group (outcome) only when the earliest window date among its member records lies in `[from, to]`. Everything else it computed belongs to a neighbouring window.
4. **Defer, never guess.** A record whose counterpart exists outside the visible range (by normalised reference, or a payout covering its provider time; looked up at the run's watermark) is classified `outside_window`, an informational class that opens no exception. Reference partners are looked up right after the reference pass, and a record with one is kept out of the later passes, exactly as a full run's reference pass would have consumed it. When the reference is unique on both sides, the counterpart is attached and rule 3 decides which window reports the pair.
5. **Scoped exception sync.** A windowed run auto-closes only open exceptions dated inside its window whose records it did not defer. Full runs behave as before.

## Consequences

Positive:

- With margins at least the largest spread of any group, consecutive windows reproduce the full run exactly (property-tested on every seed). Late settlements with a transaction time never cross a boundary, because they keep their transaction's date.
- With margins too small, a group paired by reference is reported once, as `outside_window` with both records, by the window holding its earliest record. A metric counts these, and the alert means "widen the margin".
- Storage selects windows by index on existing columns: booking time, `coalesce(txn_at, covers_from)` and `settled_on`. No stored row is rewritten.

Negative and accepted:

- Ledger and provider clocks disagree around midnight, so most groups that straddle a boundary do so by one day. The 1-day floor on margins absorbs this.
- Groups paired by amount and time, or by payout cycle, cannot be looked up when their records are further apart than the margin, and can then produce false discrepancies (RFC §6, G3). Mitigations: the floor, plus shadow comparison against full runs during rollout.
- References reused across distant records are deferred on their own and may also be paired elsewhere.
- An older binary cannot replay windowed runs: its `verify` reports MISMATCH. Flag rollback is safe. Binary rollback needs a roll-forward to re-verify.

## Alternatives considered

| Option | Why not |
|---|---|
| Window by settlement date | Splits every T+N pair near a boundary by design. The ledger has no settlement date. |
| Ledger dated by provider `paid_at` | Tightest semantics, but `paid_at` is not on the ledger row. A slack-based superset loses rows posted later than the slack. |
| One timezone for all providers | Splits bank_b's 06:00–12:00 (UTC+7) payout cycle at every UTC midnight. |
| Ownership per record | Half-reports pairs in two runs, and each half looks like a discrepancy. |
| Drop a record whose counterpart is earlier (no uniqueness check) | Loses a real `missing_in_settlement` when a reference is reused. |
| Carry unmatched state from one window to the next | Exact without margins, but makes runs depend on earlier runs. `verify` and corrections become chains. |

## Amendment (build phase, same day)

Decision 4 originally looked for counterparts only after the last pass. The property test for undersized margins showed why that is wrong. A transaction whose reference partner was outside the window stayed in the payout candidate pool that a full run would have removed it from by then, so a neighbouring group's verdict depended on the window. The lookup now runs immediately after the reference pass. The decision, "defer, never guess", is unchanged. Only the point in the pass order where it is applied moved, so that every later pass sees the same records a full run's does. See RFC §5.
