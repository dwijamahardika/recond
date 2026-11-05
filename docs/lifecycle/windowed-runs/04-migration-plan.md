# Migration plan: windowed reconciliation runs

| | |
|---|---|
| Status | Accepted |
| Schema change | `internal/adapters/sqlite/migrations/0002_windowed_runs.sql` (RFC §8) |
| Lifecycle | PRD → RFC → test plan → **migration plan** → build → rollout → launch review → cleanup |

recond migrates its schema in `sqlite.Open`: every CLI command and `serve` applies pending embedded migrations, each in its own transaction, forward only. This feature follows **expand → migrate → contract**, so the database is valid for the old binary and the new one at every step, and the flag can be flipped either way without touching the schema.

## Constraints that shape the plan

- **Inputs and runs are append-only.** Triggers reject `UPDATE` and `DELETE` on `ledger_txns`, `settlement_lines`, `webhook_events`, `runs`, `run_outcomes` and `run_members`. No input row can be backfilled with a new column. So the window date is *derived* from existing columns, and indexed through expressions (`coalesce(txn_at, covers_from)`, `upper(trim(reference))`) and partial indexes, rather than stored.
- **`ALTER TABLE ... ADD COLUMN` does not fire those triggers.** New nullable columns on `runs` are safe. Every existing run reads back with a NULL window, meaning full history, which is the truth.
- **CHECK constraints and column types cannot be altered in place.** Changing them would mean rebuilding a large, append-only table. The design needs neither.
- **`exceptions` is a mutable projection,** not append-only. Its new `window_date` column can be filled by later runs.

## Phase 1: expand (ships with the flag off)

Migration `0002_windowed_runs.sql`, one transaction:

1. `runs`: add `window_from`, `window_to` (TEXT, `YYYY-MM-DD`), `window_lookbehind`, `window_lookahead` (INTEGER). All nullable. NULL = full history.
2. `exceptions`: add `window_date` (TEXT, nullable).
3. Six indexes (RFC §8): booking time and normalised reference on `ledger_txns`; window instant, untimed `settled_on` (partial), item reference (partial) and payout period (partial) on `settlement_lines`.

What the new code does in this phase, with the flag **off**:

- every run (all full runs) writes `window_date` on the exceptions it opens or sees (dual write);
- nothing reads `window_date` yet: only windowed runs use it, and they are disabled;
- `recond migrations` (new, read-only) reports applied schema versions and how many exceptions still lack a `window_date`.

**Cost.** `CREATE INDEX` scans the table and holds SQLite's write lock for the duration. Ingestion and webhooks queue behind the migration on the in-process writer mutex. The rehearsal measures the cost on 100k-scale data. Larger databases scale roughly linearly with row count. Apply the release at a quiet time, or accept a start-up pause of that size. The indexes also add per-row work to ingestion, measured in the rehearsal as well.

**Pre-flight.** Stop writers, then copy the database file together with its `-wal` file, or use SQLite's online backup. Note the current `recond runs` output and the result digest of the latest run.

## Phase 2: migrate (still flag off)

1. Run **one full (unwindowed) run** with the new binary. It sees every current discrepancy and writes `window_date` on each exception it touches: open, still-open and reopened. This is the backfill. It needs nothing beyond the rules and records a run already loads, and it is idempotent.
2. Check with `recond migrations`. The count of **open** exceptions without `window_date` must be 0. Resolved or auto-closed legacy exceptions may stay NULL: windowed runs never auto-close NULL-dated exceptions, and if such an exception reappears, a run dates it then.
3. `recond verify` the latest pre-existing run. It must reproduce: its window is NULL, so it takes the unchanged full-history path.
4. Now the flag may be turned on ([rollout plan](05-rollout-plan.md)).

Why not a standalone backfill job: computing an exception's window date needs the provider rules (timezones) and the exception's records. That is exactly what a run already has, so a separate job would duplicate a run's loading code to save one run.

## Phase 3: contract

**There is nothing to drop from the schema.** The expand step was purely additive, renamed nothing and superseded no column. The contract step for this feature is in code: removing the feature flag and its flag-off branches, under the criteria in the [cleanup plan](08-cleanup.md). The NULL-`window_date` fallback stays even after contract. A human can reopen a legacy resolved exception, which makes an open exception with NULL `window_date`, and the cost of keeping the fallback is one comparison.

## Compatibility matrix

| Database state | Previous release binary | New binary, flag off | New binary, flag on |
|---|---|---|---|
| Schema 0001 (pre-existing) | ✓ as today | applies 0002 on open, then ✓ | applies 0002 on open, then ✓ |
| Schema 0002, no windowed runs yet | ✓. It ignores the new columns. SQLite maintains the new indexes on its inserts. Its runs and exceptions get NULL window fields, which is correct for it. | ✓ | ✓ |
| Schema 0002 with windowed runs | ✓ for everything except `verify` of a windowed run, which replays it **unwindowed** and reports **MISMATCH** (loud, not silent). Its full runs auto-close across all dates, which is right for a full run. | ✓. Windowed runs still verify. Only *creating* them is gated. | ✓ |

## Backward compatibility of runs and `verify`

- **Pre-existing runs** have NULL window columns and are replayed by today's code path: same loader, same engine call, same digest. Covered by `TestMigrationFromSchema0001`, `TestPreexistingRunsVerifyAfterWindowedRuns` and `TestUnwindowedDigestsUnchanged`, and rehearsed with a database created by the previous release binary.
- **Windowed runs** store their window. `verify` replays it with the stored rules at the stored watermarks, and looks up counterparts at the same file watermark. Inputs are append-only, so the lookup gives the same answer later.
- **Run summaries** of full runs are byte-identical to the previous release. A `window` object appears only on windowed runs.

## Rollback

| What | How | Consequence |
|---|---|---|
| **Feature** (preferred) | Set `RECOND_WINDOWED_RUNS=off` and restart. | Windowed runs can no longer be created. Existing ones stay stored and verifiable, and their exceptions stay in the queue. The next full run auto-closes across all dates, as before. |
| **Binary** | Deploy the previous release. | Works on the 0002 schema (matrix above). `verify` of windowed runs reports MISMATCH until you roll forward. Nothing is lost. |
| **Schema** | Not needed: the new columns and indexes are inert for the old binary. If the indexes alone cause trouble (ingest slowdown), `DROP INDEX` them by hand. Windowed runs then fall back to full scans, which is slow but correct. Do not delete the `0002` row from `schema_migrations`: the next start would try to re-add the columns and fail. | Only the index maintenance cost goes away. |
| **Data** | Restore the pre-flight copy. | Loses everything ingested since. Last resort only, for corruption, which this migration does not risk. |
