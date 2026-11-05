# Test plan: windowed reconciliation runs

| | |
|---|---|
| Status | Accepted. Every test below exists in the branch that implements the feature. |
| Inputs | [PRD](01-prd.md) metrics M1–M7, [RFC](02-rfc.md) guarantees G1–G3 and edge cases |
| Lifecycle | PRD → RFC → **test plan** → migration plan → build → rollout → launch review → cleanup |

## Principles

- **Test the guarantee, not the implementation.** The RFC promises that consecutive windows reproduce a full run (G1) and degrade safely when margins are too small (G2). The property tests assert exactly those statements against the full run and the generator's ground truth. They don't assert internal counters.
- **Real storage, no mocks.** As elsewhere in recond, storage tests run the real SQLite store in a temp directory. The in-memory counterpart lookup used by the engine tests is checked against the SQL one.
- **Pin known limits.** G3 (amount/time pairs further apart than the margin) is a documented limitation. A characterisation test asserts today's behaviour, so a future fix shows up as a failing test that points at the docs to update.
- **Every commit green.** `go test -race ./...`, `go vet ./...`, `gofmt -l .` and golangci-lint pass at every commit of the series, with the flag off and on.

## Risk → test map

| Risk / requirement | Test | Level |
|---|---|---|
| Window dates wrong at midnight, in UTC+7, across DST | `TestWindowDates` (table: UTC, +07:00, `America/New_York` on 1 Nov 2026, untimed line, payout) | match |
| Boundary records reported by the wrong window | `TestWindowOwnership` (table: last millisecond of `to`, first of `to+1`, ledger booked after midnight, late settlement, margin-only groups) | match |
| Counterpart beyond reach reported as missing | `TestOutsideWindow` (table: beyond lookahead, beyond lookbehind, attached when unique, alone when the reference is ambiguous, payout counterpart, no counterpart → still `missing_*`) | match |
| Window validation | `TestWindowValidate` (`to < from`, margins 0 and 32, 367-day window, zero dates) | match |
| Storage superset misses a visible record | `TestWindowBoundsCoverVisibleRange` (bounds vs per-provider instants) and `TestWindowLoadMatchesFullLoad` (SQL load + engine == in-memory engine, by digest, current and replay) | match, sqlite |
| **M1/G1: consecutive windows ≠ full run** | `TestWindowedRunsPartitionFullRun`: 8 seeds (2 with `-short`) × window sizes 1, 2, 3 and 7 days, default margins, boundary rate 5%. Asserts the multiset of groups (class, rule, members, amounts, dates, detail) across windows equals the full run's, zero `outside_window`, and the ground-truth scorer is perfect over the union. | gen (property) |
| **M2/G2: undersized margins → false or double discrepancies** | `TestWindowedRunsDeferWhenMarginTooSmall`: untimed late item credits (spread 1–5 days) with margins 1/1, 1/3 and 3/1. Asserts every full-run group appears exactly once across windows, either unchanged or as `outside_window` with the same members; 0 unexplained flags; 0 faults counted twice; and with margins 5/5, exact equality. | gen (property) |
| Scorer can't see double counting | `TestScoreCountsDuplicatesAndDeferrals` | gen |
| **M6: flag-off regression** | `TestUnwindowedDigestsUnchanged`: result digests for three seeds equal the digests captured on `main` before the feature (golden values) | gen |
| Pre-existing database breaks | `TestMigrationFromSchema0001`: build a database with migration 0001 only, insert a run with the old column list, open with the new code; the run reads back with no window and the new indexes exist | sqlite |
| **M5: pre-existing runs stop verifying** | `TestPreexistingRunsVerifyAfterWindowedRuns`: full runs with the flag off, then windowed runs with it on; every run verifies, and the old run's summary is byte-identical | app |
| Windowed run does not reproduce | `TestWindowedRunsThroughStorage`: consecutive windows through SQLite; every run verifies; the union of their `items.csv` rows equals the full run's | app |
| **Exception queue corrupted by a window** | `TestWindowedSyncScopesAutoClose` (an exception outside the window, or with NULL `window_date`, is not auto-closed) and `TestWindowedSyncKeepsDeferredOpen` (records deferred as `outside_window` keep their exception open) | sqlite |
| Exception scoping end to end | `TestWindowedRunScopesExceptionQueue`: settlements arrive late; a window run closes only its own window's exceptions | app |
| Counterpart lookup misses normalised references | `TestProberNormalisesReferences` (case and surrounding spaces, item vs payout, watermark) | sqlite |
| Flag gates creation only | `TestWindowedRunsRequireFlag` (service), CLI and HTTP flag-off cases; verify of a windowed run with the flag off still passes | app, cli, httpapi |
| CLI and API contract | `TestCLIWindowedRun`, `TestWindowedRunsOverHTTP` (201 + window in summary, 400 on invalid window and when disabled, verify reproduces, metrics exposed) | cli, httpapi |
| G3 known limit | `TestKnownLimitToleranceSpreadBeyondMargin` (characterisation) | match |

Added during the build, beyond the original plan:

| Risk | Test | Level |
|---|---|---|
| Windowed run without a prober, invalid window, unknown provider in a window | `TestWindowNeedsProberAndValidWindow` | match |
| Probe key disagrees with SQLite's `upper(trim(x))` | `TestProbeKey`, plus the check inside `TestProberNormalisesReferences` | match, sqlite |
| Exception dates differ between full and windowed runs | `TestAnchorDate` | match |
| Attaching counterparts is quadratic (found by the launch rehearsal) | `TestAttachIsAmortised`: fails at 2,773 MiB on the old code | match |
| Window lost on the run record | `TestRunWindowRoundTrip` | sqlite |
| A window query falls back to a full scan | `TestWindowQueriesUseIndexes` (pins the query plans) | sqlite |
| Deferrals invisible to monitoring | `TestOutsideWindowReachesMetrics` | app |
| Flag parsing (typos must fail at start) | `TestWindowedRunsFlag` | config |

## Generator extensions

The property tests need cases the generator did not produce:

- `UntimedItemRate`: the fraction of bank_b late item credits (`item_delay`) written without `txn_datetime`. Their window date becomes `settled_on − N`, k days after their transaction, which creates groups with spread 1–5. The default is 0, and the generator draws from its random stream only when the rate is positive, so every existing world, and every number in the README, stays the same.
- Scoring several reports at once (`gen score -items a.csv -items b.csv ...`), with the scorer recognising `outside_window` (a *deferred* fault is neither detected nor missed) and reporting a fault classified by more than one outcome as **duplicated**. "Perfect" now also requires no duplicates and no deferrals. Existing worlds must still score perfect under the stricter rule. If one does not, that's a bug the old scorer hid.

## Launch rehearsal (not a unit test)

`scripts/rehearse-windows.sh`, run by hand, results in the [launch review](07-launch-review.md):

1. Build the **previous release** from `main` and the candidate from this branch.
2. Generate four weeks of data. Ingest and run with the previous release (a pre-existing database with a pre-existing run).
3. With the candidate: verify the old run, run full, run seven-day and one-day consecutive windows, report each, verify each.
4. Compare the union of window reports with the full report (sorted rows), and score the union against ground truth.
5. Record wall time and peak RSS per run (`RECOND_STATS=1`), plus ingest time with and without the new indexes.

## Exit criteria for the build phase

- All tests above pass with `-race`, with the flag off and on.
- golangci-lint: 0 issues.
- Rehearsal: union equals full, scorer perfect, every run verifies, the old run verifies after the upgrade.
