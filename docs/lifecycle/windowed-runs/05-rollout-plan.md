# Rollout plan: windowed reconciliation runs

| | |
|---|---|
| Status | Accepted. Executed only as a local rehearsal: see the [launch review](07-launch-review.md). |
| Flag | `RECOND_WINDOWED_RUNS` (`on`/`off`). It gates creating windowed runs. Verify never needs it. |
| Lifecycle | PRD → RFC → test plan → migration plan → build → **rollout** → launch review → cleanup |

recond has no production deployment, so nothing below has run against real traffic. The plan is written the way it would be executed for a team that runs recond daily. The [launch review](07-launch-review.md) records the compressed version actually run on one machine, with real numbers. Read this as the plan, and the launch review as what happened.

## Principles

- **Dark first, then shadow, then on.** Every stage can be undone by flipping the flag. No stage needs a schema rollback.
- **Shadow on a copy.** A windowed run writes to the exception queue (opens and auto-closes). Shadow runs therefore use a *copy* of the database, so a bug found in shadow cannot touch the real queue.
- **The full run stays the reference.** Until cleanup, a weekly full run keeps running and is compared with the week's windows. Equality is the go criterion, not "looks fine".

## Stages

### Stage 0: deploy dark (flag off)

1. Pre-flight per the [migration plan](04-migration-plan.md): stop writers, back up the database file, note `recond runs` and the latest result digest.
2. Deploy. Migration `0002` applies on first open. Check `recond migrations`: it should list `0002_windowed_runs`.
3. `recond verify latest`: the pre-existing run reproduces.
4. Ingest the day's files as usual. Compare ingest wall time with the previous days. The rehearsal measured the new indexes' ingest cost; the threshold is below.

**Go to stage 1 when** verify reproduces, ingest time is within the threshold, and the daily full run's output is unchanged in shape. (New `recond_*window*` metric series read zero. That is expected.)

### Stage 1: migrate (flag off)

1. Run the daily full run. It dates every exception it sees (`window_date`).
2. `recond migrations` should report "migrate phase: complete" (no open undated exception).

### Stage 2: shadow (flag on, on a copy)

For each business day, for at least **two full weekly cycles**:

1. Copy the database after ingestion (online backup or file copy with writers stopped).
2. On the copy, with `RECOND_WINDOWED_RUNS=on`: run that day's window (`recond run -from D -to D`). At week end, also run the full week as one window.
3. On the copy, compare the union of the week's daily windows with the full run over the same data. Strip run and outcome ids, sort, diff. This is the comparison in `scripts/rehearse-windows.sh`.
4. Record per run: wall time and peak RSS (`RECOND_STATS=1`), `outside_window`, groups owned by neighbours, and records read from the margin.

**Go/no-go for stage 3:**

| Criterion | Threshold |
|---|---|
| Union of daily windows vs the full run, same data | **0 differing report rows**, every day |
| `outside_window` groups with default margins | **0**. Any non-zero count must be explained (for example untimed late lines) and fixed with a documented wider margin before go. |
| `recond verify` on every shadow windowed run | 100% reproduce |
| One-week window cost vs the full run on the same database | ≤ 50% wall time and peak RSS (PRD M3) |
| Exception sync on the copy | Each windowed run auto-closes only exceptions dated in its window. Over a week, the windows' auto-closes equal the full run's. |
| Errors in logs (`counterpart lookup`, `load inputs`) | none |

### Stage 3: on for operators (flag on in production)

1. Set `RECOND_WINDOWED_RUNS=on` for `serve` and the operators' CLI environment.
2. Daily: operators run yesterday's window and work its exceptions.
3. Weekly: the full run still runs. The comparison from stage 2 runs on a copy, and the result is posted to the team.

**Hold at stage 3 for two more weekly cycles**, with the same thresholds as stage 2, before stage 4.

### Stage 4: default on

A release flips `DefaultWindowedRuns` to `true`. `RECOND_WINDOWED_RUNS=off` remains the kill switch until the [cleanup](08-cleanup.md) criteria are met.

## Rollback

### Triggers (any one)

- A differing report row between the union of windows and the full run on the same data.
- `recond_outside_window_total` increases with default margins and no known cause.
- `recond verify` prints MISMATCH for any windowed run made by the current binary.
- A windowed run auto-closed an exception that the next full run reopens (`reopened by recond: reappeared in run N`), or `exceptions_open` drops after a windowed run by more than that window's own exception count.
- Windowed runs fail (`counterpart lookup`, `load inputs`), or exceed the full run's wall time on the same database.
- Ingest wall time regresses beyond the stage-0 threshold because of the new indexes.

### Steps

1. **Flag off:** set `RECOND_WINDOWED_RUNS=off` and restart `serve`. CLI invocations read the variable each time. New windowed runs are refused with a message naming the variable.
2. **Re-sync the queue:** run one full run. Full-run semantics apply. An exception a windowed run wrongly auto-closed but whose discrepancy still exists is *reopened by recond* with an audit trail entry. Exceptions that really cleared stay closed.
3. **Keep the evidence:** windowed runs are immutable and stay verifiable with the flag off. Attach `recond runs`, `recond verify ID --json` and the window comparison to the incident.
4. **Do not roll the binary back** unless the binary itself is broken. A previous-release binary replays windowed runs unwindowed and reports MISMATCH (migration plan, compatibility matrix).
5. **Indexes** stay, unless ingest regression was the trigger. Then the migration plan's `DROP INDEX` procedure applies.

### Thresholds to fill in from the rehearsal

The ingest-regression threshold comes from measurement, not from this plan. The [launch review](07-launch-review.md) records ingest time before and after migration `0002` on the same data. Production would use that ratio plus a margin for noise.
