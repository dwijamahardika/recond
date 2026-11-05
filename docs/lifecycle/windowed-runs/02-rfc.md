# RFC: windowed reconciliation runs

| | |
|---|---|
| Status | Accepted (key decision recorded as [ADR 0001](../../adr/0001-windowed-runs-boundary-semantics.md)) |
| PRD | [01-prd.md](01-prd.md) |
| Lifecycle | PRD → **RFC** → test plan → migration plan → build → rollout → launch review → cleanup |

## 1. Summary

A run can be given a **window**: an inclusive range of business dates `[from, to]` and two margins, `lookbehind` and `lookahead`, in days. The run loads only records whose *window date* falls in `[from − lookbehind, to + lookahead]` (the **visible range**). It matches them with the unchanged engine and **reports only the groups whose earliest record is dated inside `[from, to]`**. If a record cannot be paired because its counterpart lies outside the visible range, the run does not call it missing. It classifies it `outside_window`, attaching the counterpart when the reference is unique, and keeps it out of the later passes as a full run would. The window is stored on the run, `verify` replays it, and the exception queue is scoped so that a run over one week cannot auto-close another week's exceptions. The feature ships behind `RECOND_WINDOWED_RUNS` (default off).

## 2. How a run works today

1. `LoadInputs` reads every ledger transaction and settlement line up to the file watermark, plus every payment state.
2. `match.Reconcile` runs five passes (reference, refunded, tolerance, batch, leftovers) over sorted slices and returns outcomes. It is a pure function of its input.
3. `SaveRun` stores the run, its outcomes and members. In the same transaction, `syncExceptions` opens exceptions for new discrepancies, reopens returning ones and **auto-closes every open exception the run did not see**.
4. `verify` reloads the inputs at the stored watermarks, re-folds payment state from the event log, re-runs the engine with the stored rules and compares digests.

Two facts shape the design. Steps 3 and 4 assume a run sees *everything*. And a pair of records that belong together can carry different dates: the ledger posts after the provider (milliseconds normally, minutes on a retried webhook), a settlement arrives T+N days later, and a payout covers a six-hour cycle.

## 3. Window dates: which date of a record decides its window

Every record gets one **window date**: a civil date in **its provider's configured timezone**, the same calendar T+N already uses.

| Record | Window date | Why |
|---|---|---|
| Ledger transaction | **booking date**: `created_at` in the provider's timezone | It is on the ledger row itself, so storage can select it exactly with an index. |
| Itemised settlement line | **provider transaction date**: `txn_at` in the provider's timezone | It is the provider's own time for the transaction. A late settlement keeps its transaction's date, so "settled on T+N after the window ended" is not outside the window at all. |
| Itemised line without a transaction time (bank_b allows an empty `txn_datetime`) | `settled_on − settlement_days` | It is the latest transaction date that could have settled on time. A line that settled k days late drifts k days later. §6 covers what that costs. |
| Payout | start of the covered period (`covers_from`) in the provider's timezone | Its members' provider times all fall inside the covered period, and bank_b periods never cross local midnight. |

(Storage selects lines by `coalesce(txn_at, covers_from)`, so a payout row that does carry a `txn_at` is dated by it. The engine uses the same rule, so the two cannot disagree.)

**Windowing does not change matching.** The engine still uses the provider's clock (webhook `paid_at`) for T+N deadlines and for which payout cycle a transaction belongs to, exactly as the README's design notes require. Window dates only decide what a run reads and which run reports a group.

**Why the ledger's booking date and not the provider's `paid_at`.** `paid_at` lives in the webhook-derived payment state, not on the ledger row. Storage could only select by it by joining every ledger row to its payment, which is a full scan, the cost this feature exists to avoid. It could not select by it exactly with a cheap superset either. A ledger row posted days after the provider's time, such as a manual correction, would fall outside the slack of every window and be lost. Booking date is exact and indexable. The price is that a payment taken at 23:59:59.5 and booked at 00:00:01 has records dated on two days. Margins absorb that (§6), and a property test exercises it on every seed, because the generator deliberately places payments milliseconds before cut-offs.

## 4. Visibility and ownership

- **Visible range** `[from − lookbehind, to + lookahead]`. Records dated in it are loaded and go through every pass. Records dated outside it do not exist for this run.
- **Window** `[from, to]`. A group (outcome) is **owned** by the run when its **earliest record**, the minimum window date over its ledger and line members, is inside the window. A run reports, stores, scores and syncs exceptions for owned groups only. The rest were computed only to give boundary records their counterparts, and are counted as "not owned".

Earliest record, because it is a simple total rule every window computes the same way from the records alone. Take a pair whose settlement line is dated 7 October and whose ledger row was booked at 00:00:01 on 8 October. Week 1–7 reports it: it sees the 8th through its lookahead. Week 8–14 also sees both records through its lookbehind, recomputes the same pair, finds its earliest record on the 7th, and leaves it.

## 5. Counterpart outside the window: `outside_window`

*Amended during the build. See "Amendment" below.*

A record whose counterpart is dated outside the visible range is not a discrepancy. The run just cannot see the partner. Such records are found in two places, both bounded by the run's file watermark so `verify` asks the same questions:

1. **Right after the reference pass**, every record still unmatched is looked up by normalised reference outside the visible range: itemised lines for a ledger transaction, ledger transactions for an itemised line. The lookups are batched per provider. A record with a partner there would have been taken by a *full* run's reference pass. So it is taken here too: removed from the refunded, tolerance and batch pools, which therefore see exactly what a full run's do. When the reference is **unique on both sides** (exactly one partner, which has no other partner by reference), the partner is **attached** as a member, and the pair is reported as `outside_window` (rule `reference`) under §4's ownership rule. If the partner is *earlier* than the window, the earlier window owns the pair: it reports it with its true verdict when its lookahead reaches, otherwise as the same `outside_window` pair. This run then leaves it.
2. **After the leftovers pass**, an owned `missing_in_settlement`, `awaiting_settlement` or `missing_in_ledger` record is deferred **on its own** if its reference partner outside the window was ambiguous (step 1 found it but could not attach it), or, for a transaction, if a payout covering its provider time lies outside the visible range.

Candidates dated inside the visible range are ignored: the run already saw them. `outside_window` is informational, not a discrepancy, so it opens no exception. Its detail names the counterpart, its date and the visible range, and says which margin to widen.

Rejected alternative: dropping a record whose counterpart is earlier without the uniqueness check. With a reused reference (two ledger rows, one settlement), that drops the second row's real `missing_in_settlement` in every window.

**Amendment (build phase).** The first implementation looked for counterparts only after the leftovers pass. The G2 property test (`TestWindowedRunsDeferWhenMarginTooSmall`) failed on all 12 seed/configuration cases while G1 passed. A bank_b transaction whose untimed late credit lay beyond the lookahead was not consumed by the reference pass, so it fell into its payout's candidate pool, and the subset search had to exclude it again. Members and class were right, but the detail differed from the full run, and with tied amounts the batch could have become a false discrepancy. The cure is to follow pass precedence: the reference pass's view of the world, including partners outside the window, is settled before any later pass runs. Step 1 above is that cure.

## 6. Guarantees, and exactly when they hold

The **spread** of a group is the distance in days between its earliest and latest member's window dates in the full run.

- **G1, exact partition.** If `lookbehind ≥ spread` and `lookahead ≥ spread` for every group, consecutive windows report exactly the full run's groups, each once, with identical classes, amounts and details. No `outside_window` appears.
- **G2, safe degradation.** If a group paired by reference has a larger spread than the margins, it is reported **once, as `outside_window` with both records**, by the window holding its earliest record. It is never reported as `missing_*`, and never twice.
- **G3, what the probe cannot see.** A group paired by amount and time (mangled reference) or by payout cycle has no reference to look up. If its records are further apart than the margin, the window holding one of them can report a false `missing_in_*` or a false batch residual. This is why **margins have a floor of one day**: one day absorbs the gap between the ledger's clock and the provider's around midnight, the only way such groups spread in normal operation.

Typical spreads: **0** when every member carries the same provider instant (almost everything). **1** when the ledger books after a provider cut-off, or a tolerance pair straddles midnight. **k** for an untimed line that settled k days late. Defaults: `lookbehind = lookahead = 1`. Allowed range 1–31.

Known limits, also listed in the README:

- A reference shared by several records further apart than the margin is deferred on its own (G2 still protects it from becoming a false discrepancy), but the window may also pair the counterpart elsewhere.
- A ledger row posted more than the margin after the provider's time, inside a payout cycle, is invisible to the payout's window (G3). The rollout plan compares windowed and full runs before trusting margins.

## 7. Exception queue

Today `syncExceptions` auto-closes every open exception the run did not produce. A run over week 2 would close all of week 1's exceptions. That would be the worst bug this feature could ship, so it gets its own design:

- `exceptions` gains `window_date`: the earliest-record date of the group, written by **every** run, full or windowed (dual write during expand).
- A **windowed** run auto-closes an open exception only if its `window_date` is inside the run's window **and** none of its records sit in a group this run deferred as `outside_window`. A deferred record has no verdict yet, and closing its exception would hide it.
- An exception with `window_date` NULL predates the migration. A windowed run never auto-closes it. The next full run backfills `window_date` for every exception it sees (migration plan, "migrate" phase).
- **Full** runs keep today's behaviour exactly.

Fingerprints do not change: a group reported by a windowed run has the same records as in a full run, so it is the same exception.

## 8. Data model change (migration `0002`, additive only)

```sql
ALTER TABLE runs ADD COLUMN window_from TEXT;          -- NULL: full history (every run made before this)
ALTER TABLE runs ADD COLUMN window_to TEXT;
ALTER TABLE runs ADD COLUMN window_lookbehind INTEGER;
ALTER TABLE runs ADD COLUMN window_lookahead INTEGER;
ALTER TABLE exceptions ADD COLUMN window_date TEXT;    -- NULL: not yet seen by a run of this release

CREATE INDEX ledger_txns_created     ON ledger_txns (created_at);
CREATE INDEX ledger_txns_ref         ON ledger_txns (provider, upper(trim(reference)));
CREATE INDEX settlement_lines_window ON settlement_lines (coalesce(txn_at, covers_from));
CREATE INDEX settlement_lines_untimed ON settlement_lines (settled_on) WHERE txn_at IS NULL AND covers_from IS NULL;
CREATE INDEX settlement_lines_item_ref ON settlement_lines (provider, upper(trim(reference))) WHERE kind = 'item';
CREATE INDEX settlement_lines_payout ON settlement_lines (provider, covers_from) WHERE kind = 'payout';
```

The append-only triggers on `runs`, `ledger_txns` and `settlement_lines` fire on `UPDATE`/`DELETE`, not on `ALTER TABLE ... ADD COLUMN`, so the expand step needs no trigger changes. It also means **no input row can be backfilled**. That is why the window date is computed from existing columns and indexed through expressions rather than stored. Indexes slow ingestion, and the rehearsal measures by how much. Details in the [migration plan](04-migration-plan.md).

## 9. Loading and replay

- Storage selects a **superset**. Ledger rows have `created_at` inside the visible range's instant bounds, taken as the widest over all configured providers' timezones. Timed lines are selected by `coalesce(txn_at, covers_from)` within the same bounds. Untimed lines are selected by `settled_on` within `[from − lookbehind + min N, to + lookahead + max N]`. The engine then applies the exact per-provider rule. In-memory and SQL paths therefore produce the same visible set by construction, which a test checks.
- **Payments** are loaded only for references of loaded records: from the projection for a new run, and folded from the event log (indexed by provider and reference) for `verify`.
- `verify` of a windowed run reloads with the **stored** window and rules at the stored watermarks and probes at the same watermark. A pre-existing run has a NULL window and takes today's path unchanged.

## 10. Interfaces

CLI:

```bash
recond run -from 2026-10-01 -to 2026-10-07 [-lookbehind 1] [-lookahead 1] [-as-of 2026-10-14]
recond runs        # gains a WINDOW column only when a listed run has a window
recond verify 42   # prints the window it replayed
```

HTTP:

```http
POST /v1/runs
{"as_of": "2026-10-14T00:00:00Z",
 "window": {"from": "2026-10-01", "to": "2026-10-07", "lookbehind_days": 1, "lookahead_days": 1}}
```

Omitted or zero margins mean the default of 1. With the flag off, a request carrying `window` gets `400` with a problem explaining the flag. That is the status it got before, for an unknown field. Run summaries (`GET /v1/runs/{id}`, `summary.json`, `--json`) gain a `window` object: the window, per-provider instant bounds, in-window and margin record counts, groups not owned, and groups deferred. The summary lists the `outside_window` class only for windowed runs. A full run's summary is byte-identical to before.

## 11. Feature flag

`RECOND_WINDOWED_RUNS=on|off` (also `true/false/1/0`). It gates **creating** windowed runs only. `verify` of a stored windowed run always works, so turning the flag off is a safe rollback that strands nothing. The schema migration and the `window_date` dual write are not gated: they are the expand step, invisible to every existing command. The one flag-off change an operator can observe is on `/metrics`. The new series are registered at start-up and read zero until a windowed run happens, which follows the Prometheus advice to initialise series rather than let them appear late.

## 12. Metrics

| Metric | Meaning |
|---|---|
| `recond_windowed_runs_total` | windowed runs completed |
| `recond_last_run_window_days` | business days in the last run's window (0 = full history) |
| `recond_last_run_window_records{record, scope}` | last windowed run's records in the window vs. in the margin (`record` = ledger\|line, `scope` = window\|margin). The margin is what the window loaded but did not report. |
| `recond_last_run_window_groups_not_owned` | groups computed over the visible range but owned by a neighbouring window, i.e. excluded by the window |
| `recond_outside_window_total{provider}` | counterpart-outside-window groups. **Should stay 0 with adequate margins. Alert on increase.** |
| `recond_last_run_outside_window` | the same, for the last run |

The existing `recond_last_run_outcomes{class}` is unchanged.

## 13. Edge cases

| Case | Behaviour |
|---|---|
| Record exactly at local midnight on `to + 1` | Window date is `to + 1`: visible through lookahead, not owned. Table-tested for UTC and UTC+7. |
| DST in an IANA zone (a 25-hour day) | Dates are computed with the zone. `StartIn` handles 23- and 25-hour days. Table-tested with `America/New_York` on 1 November 2026. |
| Late settlement (T+N+k) with a transaction time | Window date is T, so it is reported in T's window as `late_settlement`. `settled_on` is irrelevant. |
| Late settlement without a transaction time | Dated k days late (§3). Inside the margin: exact. Beyond it: `outside_window` with both records (G2). |
| Ledger booked after midnight, provider paid before | Spread 1, absorbed by the 1-day floor. |
| Payout cycle crossing local midnight | Not producible by the bank_b format. In general it gives spread 1, absorbed. |
| Reference reused by several records | §6 known limits. Deferred alone, never a false discrepancy. |
| Rules change a provider's timezone between runs | `verify` uses the stored rules, so it reproduces. New runs date records by the new zone, so neighbouring windows run under different rules may overlap or leave a gap. The runbook says to re-run affected windows. |
| `as_of` before the window's deadlines | Unchanged semantics: `awaiting_settlement`. |
| Overlapping windows | Allowed. Each run is independent, and exceptions de-duplicate by fingerprint. |
| Empty window | A valid run with zero outcomes. Inputs are append-only, so no exception can be dated inside a window that holds no records, and nothing is auto-closed. |
| Provider without rules | The same configuration error as a full run, for records in the loaded range. |
| Window longer than 366 days, margins outside 1–31, `to < from` | Rejected with a validation error. |

## 14. Alternatives considered

- **A1. Window by settlement date** (the cash view: what arrived this week). Rejected. Ledger rows have no settlement date. A transaction and its late settlement would land in different windows by design, and every T+N pair near a boundary would split.
- **A2. Ledger by booking date, lines by `settled_on` with a large lookahead.** It works if the lookahead covers N plus the worst lateness, so every window loads that many extra days of settlements. A transaction time is the better key when the provider gives one. `settled_on − N` is kept only as the fallback for lines without one.
- **A3. Ledger by provider `paid_at`.** Semantically tightest (spread 0 almost everywhere), but not selectable from the ledger row (§3). A slack-based superset loses records whose posting delay exceeds the slack.
- **A4. One run-level timezone for all providers.** Rejected. In UTC, bank_b's 06:00–12:00 (UTC+7) cycle runs 23:00–05:00 and straddles every window boundary. Per-provider dates match how each provider closes its day.
- **A5. Ownership per record instead of per group.** Rejected. A pair would be half-reported by two runs and each half would look like a discrepancy.
- **A6. Drop instead of defer** (§5). Rejected: it loses real discrepancies when references are reused.
- **A7. Carry-forward state** (persist unmatched records and feed them to the next window). It would make windows cheap and exact without margins, but a run would stop being a pure function of an immutable snapshot, `verify` would have to replay chains of runs, and a correction in week 1 would invalidate every later window. Not now.
- **A8. Archival or partitioning by month.** Orthogonal: it bounds storage, not what a run reads. Possibly later.

## 15. Rollout (detail in [05-rollout-plan.md](05-rollout-plan.md))

1. Ship with the flag **off**. The migration runs on start (expand), and every run dual-writes `exception.window_date`. Nothing visible changes.
2. One full run backfills `window_date` (migrate).
3. **Shadow**: windowed runs alongside the existing full run, compared by script. Go/no-go is equality.
4. Flag on for operators, then default on. The flag stays as a kill switch until the [cleanup](08-cleanup.md) criteria are met.

## 16. Risks

| Risk | Mitigation |
|---|---|
| A windowed run auto-closes another window's exceptions | §7 scoping, legacy NULL never closed by a windowed run, and a dedicated test |
| Margin too small, giving false discrepancies (G3) | 1-day floor, `outside_window` metric and alert, shadow comparison before go-live |
| Six new indexes slow ingestion | Measured in the rehearsal. Ledger and settlement ingest are a small share of total ingest, which webhooks dominate. |
| Binary rollback below this release | Windowed runs are stored with their window, but an older binary does not read it: its `verify` replays them unwindowed and reports **MISMATCH** (loud, not silent). Flag rollback is the safe path. The rollout plan says so. |
| Engine semantics drift between windowed and full | The property test asserts equality with the full run on every seed, plus a golden-digest test for unwindowed runs. |
| Lookup cost on large runs | One batched lookup per provider after the reference pass, covering every record still unmatched. For a payout provider that is almost all of its transactions. Indexed, chunked `IN` queries, measured in the rehearsal. Payout lookups run only for owned leftovers. |

## 17. Testing (detail in [03-test-plan.md](03-test-plan.md))

Table tests for window dates and boundary instants (UTC, UTC+7, DST), ownership, deferral and attachment. A property test on the generator: consecutive windows of 1, 2, 3 and 7 days reproduce the full run, and with undersized margins every group appears once, as itself or as `outside_window`. An end-to-end test through SQLite. Upgrade tests from a schema-0001 database. A golden digest for unwindowed runs. Exception scoping tests. CLI and HTTP tests with the flag on and off.
