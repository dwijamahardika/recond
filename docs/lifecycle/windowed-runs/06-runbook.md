# Runbook: windowed reconciliation runs

For whoever is on call for recond. Background: [RFC](02-rfc.md), [ADR 0001](../../adr/0001-windowed-runs-boundary-semantics.md).

## In one paragraph

`recond run -from D1 -to D2` reconciles business dates `D1..D2`. Dates are calendar dates in **each provider's own timezone**: bank_b's 1 October starts at 17:00 UTC on 30 September. The run reads `lookbehind` and `lookahead` extra days (the **margin**, default 1 each) to find counterparts, and **reports only groups whose earliest record is dated in the window**. A record whose counterpart is dated beyond the margin becomes `outside_window`. That is not a discrepancy and opens no exception, and it means "this run could not decide; widen the margin". A ledger row is dated by its booking time. A settlement line is dated by the provider's transaction time, by `settled_on − N` when it has none, or, for a payout, by the start of the period it covers.

## Daily use

```bash
export RECOND_WINDOWED_RUNS=on
recond run -from 2026-10-07 -to 2026-10-07            # yesterday; margins default to 1 day
recond report -out reports                            # items.csv + summary.json of the latest run
recond exceptions                                     # the queue: only real discrepancies
recond verify latest                                  # replays with the stored window
recond runs                                           # WINDOW column shows each run's window
```

Read the run's window block. Example (a generated world of 3,000 payments over five days):

```
window 2026-10-01..2026-10-03 (lookbehind 1d, lookahead 1d): 3 business day(s), each provider's own calendar
  bank_b   +07:00  2026-09-30T17:00:00Z .. 2026-10-03T17:00:00Z
  psp_a    +00:00  2026-10-01T00:00:00Z .. 2026-10-04T00:00:00Z
  dated in the window: 1,637 ledger txns, 991 settlement lines; read from the margin: 570 / 372
  groups owned by neighbouring windows: 377; outside_window (counterpart beyond the margin): 0
```

The last line is what matters: `outside_window` should be **0**.

## Metrics

| Metric | Meaning | Normal |
|---|---|---|
| `recond_windowed_runs_total` | windowed runs completed | grows by your schedule |
| `recond_last_run_window_days` | business days in the last run's window; 0 after a full run | 1 for dailies, 7 for weeklies |
| `recond_last_run_window_records{record,scope="window"}` | ledger rows / lines dated in the last window | tracks daily volume |
| `recond_last_run_window_records{record,scope="margin"}` | rows read only to find counterparts, not reported | about 2 days' volume with 1-day margins. For a one-day window that is more than the window itself; that is the cost of correctness, not a fault. |
| `recond_last_run_window_groups_not_owned` | groups computed for boundary records, owned by the neighbouring window | non-zero is normal |
| `recond_outside_window_total{provider}` | groups deferred because a counterpart is beyond the margin | **flat** |
| `recond_last_run_outside_window` | the same, for the last run | **0** |
| `recond_last_run_outcomes{class}`, `recond_exceptions_open`, `recond_run_duration_seconds` | unchanged from before windowed runs | |

## Alerts

| Alert | Expression | Severity | First step |
|---|---|---|---|
| **OutsideWindowGroups** | `increase(recond_outside_window_total[1d]) > 0` | warning | Failure mode A |
| **ExceptionsDroppedAfterWindowedRun** | `delta(recond_exceptions_open[30m]) < -50 and increase(recond_windowed_runs_total[30m]) > 0` (tune 50 to the window's usual size) | warning | Failure mode C |
| **WindowedRunSlow** | `histogram_quantile(0.95, rate(recond_run_duration_seconds_bucket[1d])) > <full-run p95>` | info | Failure mode E |
| **WindowedVerifyFailed** | scheduled `recond verify latest` exits non-zero (the CLI exits 1 on MISMATCH) | page | Failure mode B |

## Failure modes

### A. `outside_window` groups appear

**Meaning:** a record in the window has a partner dated beyond the margin. The run did not guess. The verdict is pending.

```bash
recond report -out reports                                         # latest run
grep ',outside_window,' reports/run-<ID>/items.csv | head          # which records, and the detail
```

The detail names the counterpart, its date and the margin to widen: `counterpart outside window: settlement line bank_b:… dated 2026-10-11, visible range 2026-09-30..2026-10-08 (attached); verdict deferred (pairs by reference); widen -lookahead to decide here`.

Usual causes, most likely first:

1. **Settlement lines without a transaction time that settled late** (bank_b allows an empty `txn_datetime`). They are dated `settled_on − N`, so k days late means dated k days later. Fix: re-run the window with `-lookahead k`.
2. **Ledger rows booked days after the provider's time** (manual postings). The detail gives the counterpart's window date, which for a ledger row is its booking date. Compare it with `paid_at` from `recond payment PROVIDER REF`. (`outside_window` is not a discrepancy, so no evidence is stored for it.) Fix: `-lookbehind` or `-lookahead` to cover the delay.
3. **Reused references.** The detail says `not attached: reference not unique`. Look the reference up with `recond payment`. This is a data problem upstream, not a window problem.

Re-running a wider window is safe. Runs are immutable and the exception queue de-duplicates by fingerprint. Exceptions of records deferred as `outside_window` are **kept open** (counted as `kept_open_deferred` in the run's JSON summary, `recond run -json` or `summary.json`) until a run can decide.

### B. `verify` reports MISMATCH for a windowed run

```bash
recond verify ID -json        # window, input_matches, digests, engine_version vs current_engine
recond runs -json             # engine_version, window and created_at of every run
```

- `engine_version` differs from `current_engine`: the engine changed between the run and now. Expected after an upgrade that bumps it. Re-run the window if you need a current verdict.
- **The binary is the previous release** (no window support): it replays windowed runs unwindowed. Roll forward. Nothing is lost.
- `input_matches: false`: the stored inputs changed under a watermark. Inputs are append-only, so this means the database was restored or edited outside recond. Escalate. Do not trust any run on this database until explained.
- Inputs match but the result does not, on the same engine version: a determinism bug. Roll the flag back ([rollout plan](05-rollout-plan.md#rollback)) and keep the database copy for analysis.

### C. Exceptions closed unexpectedly after a windowed run

```bash
recond exceptions -status auto_closed -limit 20
recond exceptions show ID     # audit trail line: "auto_closed  by recond  not present in run N"
recond runs                   # run N's WINDOW
```

A windowed run may close only exceptions dated inside its window, and never one whose records it deferred. If one outside the window was closed, that is a scoping bug: roll the flag back. The next **full** run reopens every closed exception whose discrepancy still exists (`reopened by recond: reappeared in run M`), so the queue repairs itself.

### D. A windowed run refuses to close old exceptions

`recond migrations` shows the number of **undated** exceptions, created before this release and not yet seen by a run of it. Windowed runs never auto-close those. One full run dates every exception it sees.

### E. A windowed run is slow or uses too much memory

- `recond migrations` must list `0002_windowed_runs`. Without its indexes a window is a full scan.
- Compare `recond_last_run_window_records{scope="margin"}` with `scope="window"`. A one-day window reads about three days with the default margins. Run longer windows if cost matters more than granularity.
- The run's `timings:` line splits load, match, evidence and save. After a week of `outside_window` investigations, a large `-lookahead` multiplies `load`.

### F. "This transaction is in the wrong window" (timezones)

The run prints each provider's window as UTC instants. A ledger row's date is its **booking time** in its provider's timezone. A payment the provider took at 23:59:59 and the ledger booked at 00:00:01 has its records dated on two days. The pair is reported by the earlier day's window, and the later window, which also sees both, leaves it. Matching (T+N, payout cycles) still uses the provider's clock.

### G. `windowed runs are disabled (set RECOND_WINDOWED_RUNS=on)`

The flag is off for that process. HTTP returns `400 Windowed runs disabled`. An invalid value (`maybe`) makes every command fail at start, by design.

## Rollback (summary)

`RECOND_WINDOWED_RUNS=off`, restart `serve`, run one full run. Details and triggers: [rollout plan](05-rollout-plan.md#rollback).
