# Launch review: windowed reconciliation runs

| | |
|---|---|
| Status | **Local rehearsal, not a production launch.** recond has no production deployment and no users. Everything below ran on one machine against generated data. |
| Date | 2026-11-05 |
| Decision | **Go**: the flag default flipped to on ("feat: enable windowed runs by default"). `RECOND_WINDOWED_RUNS=off` stays as the kill switch ([cleanup](08-cleanup.md)). |
| Lifecycle | PRD → RFC → test plan → migration plan → build → rollout → **launch review** → cleanup |

## What was rehearsed, and what was not

The [rollout plan](05-rollout-plan.md) calls for weeks of shadow runs on copies of a production database. None of that exists here. What ran instead, compressed onto one machine:

| Rollout stage | Rehearsed as |
|---|---|
| 0, deploy dark | The previous release (`main`) created a database and a full run. The candidate opened it, applied migration `0002`, and verified the old run. |
| 1, migrate | The candidate's full run, then `recond migrations` reported "migrate phase: complete". |
| 2, shadow | Consecutive weekly and daily windows compared with the full run over the same data, scored against the generator's ground truth, and every run verified. Once, not over weeks. |
| 3/4, on, then default on | Runbook drills on the real CLI: margin too small, rollback, previous binary on the new schema. Then the default flip and post-flip checks. |

Not rehearsed: real data, real traffic, a multi-week shadow period, alerts wired into a real Prometheus, the HTTP path under load (covered by tests only), and 1M-payment scale (skipped to keep the shared machine's load modest; the README's 1M benchmarks are for full runs).

## Environment

4 vCPU cloud VM (Intel Xeon Processor @ 2.10GHz), 16 GiB RAM, **shared with other jobs**. Linux, Go 1.24.7, `modernc.org/sqlite` v1.46.1, WAL, 256 MiB page cache. Each stage ran as its own process. Wall time, CPU time and peak RSS come from `getrusage` (`RECOND_STATS=1`). Cost figures are medians of 3. Everything else is single runs. Ad-hoc reruns of the same weekly window before the fix ranged 175.6–185.6 MiB peak, so differences of a few percent are noise.

## Procedure

```bash
make rehearse        # = N=100000 DAYS=28 SIZES="7 1" REPEAT=3 scripts/rehearse-windows.sh
```

Data: `gen -n 100000 -seed 1 -days 28` gives 94,756 ledger transactions, 56,236 settlement lines in 62 files, 345,448 webhook deliveries and 1,663 injected faults, reconciled as of 2026-11-05. The script ran twice: rehearsal #1 on the candidate as first built, rehearsal #2 after the fix described under "What went wrong".

## Results

### Correctness (identical in both rehearsals)

| | Windows | Report rows differing from the full run | Ground truth over the union | `outside_window` | Verified |
|---|---:|---:|---|---:|---:|
| Weekly (7 days, margins 1/1) | 5 | **0** of 151,485 | 1,663 of 1,663 faults, 0 unexplained | 0 | 5/5 |
| Daily (1 day, margins 1/1) | 29 | **0** of 151,485 | 1,663 of 1,663 faults, 0 unexplained | 0 | 29/29 |

The weekly windows' open exceptions (366 + 369 + 353 + 342 + 0) add up to the full run's 1,430 discrepancies: each exception was seen exactly once. ("pairs checked 0" in the CLI scorer's output is expected, because `truth.json` does not serialise the pairing map. The Go property tests check pairing.)

### Upgrade from the previous release (rehearsal #2; #1 in brackets)

| Stage | Wall | Peak RSS | Result |
|---|---:|---:|---|
| previous release: ingest | 9.99 s (9.95) | 418 MiB | |
| previous release: full run #1 | 2.75 s (2.93) | 367 MiB | |
| candidate: first open, migration `0002` (six indexes built) | 0.45 s (0.41) | 97 MiB | |
| candidate: verify run #1 | 2.07 s (2.12) | 369 MiB | reproduced |
| candidate: full run #2 | 2.78 s (2.78) | 355 MiB | **result digest identical to run #1** |

### Cost: one window vs the full run on the same 28-day database (median of 3)

| | Rehearsal #1 wall · peak RSS | Rehearsal #2 wall · peak RSS | Share of full (#2) | PRD target | Ledger rows read |
|---|---|---|---|---|---:|
| Full run | 2.912 s · 367.4 MiB | 2.918 s · 356.6 MiB | | | 94,756 |
| 7-day window | 1.266 s · 185.5 MiB (43% · **50.5%**) | 1.046 s · 144.9 MiB | **35.8% · 40.6%** | M3 ≤ 50% · met in #2 | 30,489 (32%) |
| 1-day window | 0.363 s · 80.7 MiB (12% · 22%) | 0.318 s · 60.0 MiB | **10.9% · 16.8%** | M4 ≤ 25% · met | 10,169 (11%) |

A one-day window reads three days of records with 1-day margins: 3,442 ledger rows dated in the window and 6,727 in the margin. Re-covering history window by window costs more in total than one full run did: 4.3 s for the five weekly runs and 8.7 s for the 29 daily runs, against 2.9 s. The gain is per-run cost and memory that do not grow with the history the database holds. A full run's cost does grow with it. That scaling claim follows from the loading design and was not measured beyond 28 days here.

### Ingest cost of the six new indexes (same files, fresh databases, single runs)

| | previous release | candidate | Δ |
|---|---:|---:|---:|
| Rehearsal #1 | 10.92 s · 416 MiB | 11.30 s · 433 MiB | +3.5% |
| Rehearsal #2 | 10.62 s · 416 MiB | 11.24 s · 432 MiB | +5.8% |

Ingest is dominated by the 345k-event webhook backfill, which gained no index. For the rollout plan's stage-0 threshold, a regression above about 10% would be a signal rather than noise.

### Runbook drills (CLI, 20,000 payments over 14 days, every `item_delay` credit written without a transaction time)

| Drill | Command | Observed |
|---|---|---|
| A1, margin too small | `run -from 2026-10-01 -to 2026-10-07` | 20 `late_settlement`, **7 `outside_window`** with both records attached, 0 `missing_*`. Warning log: `windowed run deferred groups whose counterpart is outside its margins`. Detail: `… dated 2026-10-10, visible range 2026-09-30..2026-10-08 (attached); … widen -lookahead`. |
| A2, follow the runbook | the same week with `-lookahead 5` | 27 `late_settlement`, 0 `outside_window`. 7 exceptions opened. Verified. |
| A3, consecutive weeks | three weeks, default margins vs `-lookahead 5`, scored together | Default: of 57 faults, 42 detected, **15 deferred, 0 missed, 0 unexplained, 0 duplicated**. `-lookahead 5`: 57 of 57, perfect. |
| B, rollback | `RECOND_WINDOWED_RUNS=off` | Windowed run refused (`windowed runs are disabled (set RECOND_WINDOWED_RUNS=on)`, exit 1). All 8 windowed runs still verify. The full run sees the same 57 exceptions (0 opened, 57 still open). |
| C, previous binary on the new schema | `prev/recond verify 1`, `prev/recond run` | Verifying a windowed run gives **MISMATCH**, exit 1 (loud, as the migration plan says). Its own full runs work on schema 0002 and verify with either binary. |

### Post-flip checks (default on, variable unset)

`make demo` output is unchanged, including result digest `779b2d283f5b2b28`, the value in the README. `run -from 2026-10-01 -to 2026-10-07` on the demo database runs without setting the variable and verifies. The full test suite passes under the race detector with the new default.

## PRD scorecard

| # | Metric | Result |
|---|---|---|
| M1 | Partition correctness | **Met**: property test on 8 seeds × 4 sizes, end-to-end through SQLite, and the rehearsal (0 differing rows of 151,485) |
| M2 | No false discrepancies when margins are too small | **Met**: property test (12 seed/config cases × 3 margin pairs × 2 sizes) and drill A3 |
| M3 | Weekly window ≤ 50% of full | **Met in #2** (35.8% wall, 40.6% RSS). **Missed in #1** on RSS (50.5%), see below |
| M4 | Daily window ≤ 25% of full | **Met** (10.9% wall, 16.8% RSS) |
| M5 | Windowed and pre-existing runs verify | **Met**: 34/34 windowed, plus the previous release's run after upgrade |
| M6 | Flag-off digests unchanged | **Met**: golden test on three seeds; at 100k, the candidate's full-run digest equals the previous release's; demo digest unchanged |
| M7 | Self-explaining output | **Met**: CLI window block, summary JSON `window`, six metrics; used in drill A |

## What went wrong

Before the rehearsal, during the build:

1. **The first deferral design was wrong, and the G2 property test said so.** It failed on all 12 seed/config cases while G1 passed. A transaction whose reference partner lay beyond the margin fell into its payout's candidate pool, so a neighbouring group's verdict depended on the window. The design was amended so lookups happen right after the reference pass (RFC §5 amendment, ADR 0001 amendment). This is the most important finding of the whole exercise, and it came from the property test, not a review.
2. **The CLI silently turned `-lookbehind 0` into 1.** The service reads a zero margin as "default" (right for omitted JSON fields, wrong for a flag whose default is already 1). Caught by the CLI test. The CLI now validates the window itself.
3. **A windowed summary printed "647 of 646 ledger txns matched (100.15%)".** It counted matched members of owned groups over records dated in the window. A payout member booked after midnight is dated in the margin but belongs to a group the window owns. Spotted by running the CLI by hand. Summaries now count reported records.
4. **Two commits went in with gate findings.** One had two staticcheck QF1008 hints, fixed in the CLI commit. One had a gofmt misalignment in a test, fixed in the next commit ("style(match): gofmt window_test.go"). The gate script's exit status was lost in a pipe. The gate was fixed, and history was not rewritten.

During the rehearsal:

5. **Rehearsal #1 missed M3 on peak RSS: 185.5 MiB, 50.5% of the full run, while reading 32% of the rows.** An allocation profile of the weekly run put 258 MB of 424 MB allocated in `attach`. To protect a caller's spare capacity it re-sliced the input to its length on *every* attachment, copying the whole record slice each time. That is quadratic in attachments. Most attachments are margin records whose partner lies just beyond the visible range, and their groups are dropped as owned by the neighbour, so `outside_window` stayed 0 and only the cost showed it. Fixed in "perf(match): attach outside counterparts with amortised appends", with a regression test that allocates 2,773 MiB on the old code. Rehearsal #2: 144.9 MiB, 40.6%. Nothing else changed between the two rehearsals.

Nothing went wrong in the upgrade, verify, rollback or previous-binary drills.

## Decision and follow-ups

**Go**, for this repository's purposes: correctness held at every level tested, cost targets are met, and rollback is a flag that strands nothing. A real launch would still run the multi-week shadow stage first.

Follow-ups, not blocking:

- The tolerance/payout spread limit (RFC §6, G3) has no runtime signal. Only the weekly full-run comparison would catch it. Consider a cheap check: an unmatched record in a window whose amount and time match a record just beyond the margin.
- The daily window reads twice its own volume from the margins. If daily cost matters more, a smaller default margin is not available (1 day is the floor), but storing a provider-time column for ledger rows at ingest would allow tighter windows. That is a schema change for another RFC.
- Measure at 1M payments when the machine is not shared.
