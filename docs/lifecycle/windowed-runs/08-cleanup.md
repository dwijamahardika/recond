# Cleanup: the `RECOND_WINDOWED_RUNS` flag

| | |
|---|---|
| Status | **Decision: keep the flag for now.** Removal criteria and the exact removal checklist are below. |
| Lifecycle | PRD → RFC → test plan → migration plan → build → rollout → launch review → **cleanup** |

## What is left to clean up

The schema side needs no contract step. Migration `0002` was purely additive: it renamed nothing and superseded no column (see the [migration plan](04-migration-plan.md#phase-3-contract)). The NULL-`window_date` fallback in the exception sync stays permanently. A human can reopen an exception created before this release, making an open exception that no run has dated yet, and the fallback costs one comparison.

That leaves the **feature flag**. Since the [launch review](07-launch-review.md) it defaults to on, and `RECOND_WINDOWED_RUNS=off` is the kill switch.

## Decision: keep it, and why

**Removing it now would be premature.** A kill switch is worth most during the first weeks of real use, and recond has had none. The "launch" was a local rehearsal on generated data: one run of each check, on one machine. The rehearsal found a performance bug that every test had missed. The next surprise is more likely to come from real data (references reused across weeks, ledger postings days late, the G3 limit on amount/time pairs) than from anything a generator produces. When that happens, the cheapest safe response is a one-variable rollback that strands nothing. That is exactly what the flag is.

**Keeping it is cheap.** It is one env var, one service option, one check in `Service.Run`, one HTTP problem response, and tests for the off path. It does not fork the engine. Windowed and full runs share every pass, and the flag only gates *requesting* a window.

**The case for removing it sooner is real, and is noted here.** Windows are opt-in per request: nobody gets a windowed run without asking for `-from/-to`. So once operators trust windows, the flag's remaining value is limited to HTTP clients, which can create windowed runs that touch the shared exception queue. If the API stays internal to the operators, criterion 6 below can be waived.

## Criteria for removal

All of these must hold, measured in the real environment (production or the team's equivalent), not in a rehearsal:

1. **Four consecutive weekly closes** using windowed runs as the primary close, each compared with that week's full run on a copy of the database: **0 differing report rows** every week.
2. **`recond_outside_window_total` flat** with the configured margins, or every increase explained (for example untimed late lines) and resolved by a documented margin change.
3. **No kill-switch use** in that period. If the flag is turned off for a windowed-runs problem, root-cause it, add a test, and restart the four-week clock.
4. **Every windowed run verifies** (scheduled `recond verify latest` green) in that period.
5. **At least one release upgrade** in that period went out without touching the flag. That shows the flag is not part of routine operations.
6. **The G3 follow-up is decided**: either a runtime signal exists for amount/time or payout pairs further apart than the margin, or the team explicitly accepts the weekly full-run comparison as the only signal. (May be waived if the HTTP API is operator-only; see above.)

## Removal checklist (the code "contract" step)

When the criteria hold, one commit, `chore: remove the windowed-runs feature flag`, deletes:

| Where | What |
|---|---|
| `internal/config/config.go` | `DefaultWindowedRuns`, `WindowedRuns()`, `Config.WindowedRuns`, the call in `Load` |
| `internal/config/config_test.go` | `TestWindowedRunsFlag` |
| `internal/app/service.go` | `Service.windowed`, `WithWindowedRuns`, `WindowedRunsEnabled`, `ErrWindowedRunsDisabled` |
| `internal/app/run.go` | the flag check at the top of `Run` |
| `internal/adapters/httpapi/handlers.go` | the "Windowed runs disabled" problem and the `ErrWindowedRunsDisabled` case |
| `cmd/recond/main.go`, `serve.go`, `commands.go` | reading the flag in `open`, the `feature flags` log line, the flag line of `recond migrations` |
| tests | the flag-off cases in `TestWindowedRunsRequireFlag`, `TestPreexistingRunsVerifyAfterWindowedRuns` (keep the verify part), `TestCLIWindowedRun`, `TestWindowedRunsOverHTTP`; `app.WithWindowedRuns(true)` in test setup |
| `.env.example`, README | the `RECOND_WINDOWED_RUNS` entries |
| docs | runbook failure mode G and rollback summary; rollout plan rollback step 1 becomes "stop requesting windows; run a full run". Add a dated note to this document saying the flag was removed. |

After removal, rollback means "stop requesting windows, run a full run". Windowed runs already stored keep verifying, because verify never depended on the flag.
