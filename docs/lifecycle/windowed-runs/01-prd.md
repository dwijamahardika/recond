# PRD: windowed reconciliation runs

| | |
|---|---|
| Status | Approved for design (RFC: [02-rfc.md](02-rfc.md)) |
| Owner | recond maintainer |
| Lifecycle | **PRD** → RFC → test plan → migration plan → build → rollout → launch review → cleanup |

This is the first document in a worked example of taking one feature from problem to post-launch. recond is a portfolio project with no production deployment and no real users, so the users below are personas drawn from how finance operations teams work, and "launch" later means a local rehearsal, said plainly in the [launch review](07-launch-review.md).

## Problem

Every `recond run` reconciles **everything ever ingested**. The README says so under "not done": runs have no date windows and no archival. That was the right first cut: one run, one answer, no boundaries to get wrong. It stops working as history accumulates:

1. **Cost grows with history, not with the question.** Finance closes a week at a time. On a database holding a quarter, closing one week still loads, matches, stores and reports thirteen weeks. The README's benchmarks show where that goes: at 1M payment attempts a run peaks at 2.48 GiB and spends most of its time loading inputs and storing results. Both grow with history.
2. **The report answers the wrong question.** A weekly close needs "for transactions of 1–7 October: what matched, what is wrong, what is still due". A full run mixes the week in with every older week, so the analyst has to filter `items.csv` by hand and decide on their own where each boundary item belongs.
3. **There is no reproducible record of what was closed.** A run stores its input watermarks but not what period it was meant to cover. When an auditor asks how week 40 was closed, nothing in the run says it covered week 40.

## Users

| Persona (illustrative) | Job to be done | Today |
|---|---|---|
| **Finance operations analyst** | Close a business week or day: work every discrepancy for that period, then sign off. | Runs everything, filters the report by date in a spreadsheet, and argues about items near midnight. |
| **Finance controller / auditor** | Re-check later exactly what a past close saw and decided. | `recond verify` proves a run reproduces, but the run does not say which period it covered. |
| **Platform engineer running recond** | Keep runs inside their memory and time budget as the database grows. | Run cost grows linearly with history. The only lever is a bigger machine. |

## What "done" looks like for the user

- `recond run -from 2026-10-01 -to 2026-10-07` reconciles only that week's **business dates**. Over HTTP, `POST /v1/runs` takes the same window as JSON.
- Business dates follow the provider's own calendar (`psp_a` in UTC, `bank_b` in UTC+7), the same calendar T+N settlement terms already use. "1 October" means 1 October where the provider is.
- **Items near the boundary land in exactly one week's report.** A transaction on 7 October that settles on 9 October (T+2) belongs to week 1–7, and its settlement is found even though it arrived after the window ended. A transaction posted at 00:00:01 for a payment the provider took at 23:59:59 is not reported twice and not lost.
- When recond cannot decide inside the window, it says so with its own class, `outside_window`, which names the counterpart and its date. It never reports such an item as a missing settlement.
- The window is stored on the run. `recond verify` reproduces a windowed run exactly, and runs made before the feature still verify.
- Closing seven consecutive days or one week gives the same answers as one full run over the same data. Every discrepancy appears once.

## Success metrics

Targets are set here, before building, and measured in the [launch review](07-launch-review.md).

| # | Metric | Target | How measured |
|---|---|---|---|
| M1 | **Partition correctness:** consecutive windows with default margins reproduce the full run's outcomes (same groups, classes, amounts, details) | 100% on every generated seed; 0 `outside_window` | property test over the ground-truth generator; end-to-end test through SQLite; local rehearsal at 100k payments |
| M2 | **No false discrepancies from windowing:** when the margin is too small, affected items become `outside_window`, never `missing_*`, and no discrepancy is counted twice | 0 unexplained flags, 0 double-counted faults | property test with deliberately undersized margins |
| M3 | **Cost follows the question:** on a database holding four weeks, a one-week window | ≤ 50% of the full run's wall time and peak RSS | rehearsal, `RECOND_STATS=1` (getrusage) |
| M4 | same, one-day window | ≤ 25% of the full run's wall time and peak RSS | rehearsal |
| M5 | **Reproducibility:** windowed runs, and runs made before the upgrade, verify | 100% | tests; rehearsal with a database created by the previous release |
| M6 | **No regression with the flag off:** unwindowed runs produce byte-identical result digests to the previous release | identical | golden-digest test against digests captured on `main` |
| M7 | **Self-explaining output:** the run summary says how many records were in the window, in the margin, and deferred | present in CLI, JSON and metrics | review |

## Scope

In scope:

- Inclusive business-date windows `[from, to]` with a configurable **lookbehind** and **lookahead** margin in days.
- Window stored on the run record. `verify` replays it.
- CLI flags, HTTP API field, run summary, report and Prometheus metrics.
- An `outside_window` class for an item whose counterpart exists but lies outside what the window can see.
- An exception queue that a windowed run cannot corrupt: a run over week 2 must not auto-close week 1's exceptions.
- A feature flag (`RECOND_WINDOWED_RUNS`, default off) so the feature ships dark and can be switched off without a deploy.

## Non-goals

- **Carry-forward state between runs** (persisting "still unmatched" from one window into the next). Each run stays a pure function of an immutable input snapshot. See RFC alternative A7.
- **Archival or deletion** of old inputs. Windows bound what a run *reads*. The database still grows.
- **Business-day calendars** (weekends, bank holidays). Windows and margins are calendar days, as T+N already is.
- **Changing matching rules.** Windowing decides what a run looks at and which run reports a result. It must not change how two records are matched.
- **Postgres, multi-replica, per-merchant windows, a UI.**

## Constraints

- Inputs and runs are append-only and enforced by SQLite triggers. The design cannot rely on updating stored rows.
- Unwindowed behaviour is the contract: digests, reports and API responses for full runs must not change.
- The engine stays a pure function of its input. Anything windowing needs from storage is injected.

## Open questions → answered in the RFC

1. Which date of a record decides its window: ledger booking time, provider transaction time, or settlement date? (RFC §3, ADR 0001)
2. Who reports a group whose records fall on both sides of a boundary? (RFC §4)
3. What happens when the margin is too small? (RFC §5)
4. How does the exception queue avoid auto-closing exceptions outside the window? (RFC §7)
