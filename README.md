# recond

A **payment reconciliation engine** in Go. Where the companion `ledgerd` repo keeps the books, recond answers the question finance teams ask every morning: *does what our ledger says match what the payment provider and the bank actually settled — and if not, exactly which transactions are wrong, and why?* It ingests a ledgerd-style transfer export, settlement files in each provider's own format, and real-time payment-status webhooks that arrive duplicated and out of order. It matches 1:1 by reference, falls back to amount-and-time tolerance when references are mangled, and solves N:1 payouts that cover many transactions minus fees. Every discrepancy lands in an exception queue with evidence and an append-only audit trail. Runs are versioned and replayable, and a ground-truth property test holds the whole thing to account.

```
ledger export (ledgerd-style) ───────────────┐
psp_a settlement CSV ──► psp_a adapter ──────┤                  ┌─ matched (reference | tolerance | batch | refunded)
bank_b statement CSV ──► bank_b adapter ─────┼─► SQLite ─► run ─┤
webhooks (HMAC, duplicated, out of order) ───┘   (immutable     └─ amount_mismatch · fee_mismatch · late_settlement
        └─► payment state machine                 inputs)          duplicate_settlement · missing_in_settlement
                                                                   missing_in_ledger ──► exception queue + audit trail
```

All providers, formats, fee rates and data in this repository are **fictional**. `psp_a` and `bank_b` are made-up providers with deliberately different file layouts. Their fee rules are illustrative examples, not anyone's real pricing. Generated data is synthetic.

## Quickstart

```bash
make demo        # generate a faulty world, reconcile it, score it against ground truth, work an exception
```

Output from a real run, trimmed:

```
$ gen -out demo-out/data -n 5000 -seed 42
wrote demo-out/data in 104ms: 4732 ledger txns, 2882 settlement lines in 19 files, 17238 webhook deliveries
injected 77 faults: amount_change=11 batch_delay=1 batch_fee=2 corrupt_reference=9 delay=10 drop_ledger=16 drop_settlement=9 duplicate=10 fee_change=8 item_delay=1

$ recond ingest -dir demo-out/data
FILE                             KIND      ROWS    INSERTED  DUPLICATE  CONFLICT  REJECTED  NOTE
ledger.csv                       ledger    4,732   4,732     0          0         0
bank_b_statement_2026-10-02.csv  bank_b    3       3         0          0         0
...
webhooks.jsonl                   webhooks  17,238  15,014    2,224      0         0         923 out of order, 40 illegal transitions

$ recond run -as-of 2026-10-13T00:00:00Z
                  CLASS  GROUPS  LEDGER  LINES        |NET DIFF|
                matched   2,869   4,501  2,823
        amount_mismatch      11      11     11    243,712.63 IDR
           fee_mismatch      10     130     10     28,133.00 IDR
        late_settlement      12      81     12          0.02 IDR
   duplicate_settlement      10      10     10  4,103,869.48 IDR
  missing_in_settlement       9       9      0  8,187,538.65 IDR
      missing_in_ledger      16       0     16  7,750,816.23 IDR
exceptions: 68 opened, 0 reopened, 0 auto-closed, 0 still open, 0 already resolved

$ gen score -truth demo-out/data/truth.json -items demo-out/reports/run-1/items.csv
faults 77, detected 77; flagged outcomes 68, unexplained 0
every injected fault classified exactly; nothing clean flagged

$ recond verify 1
  result  779b2d283f5b2b28  replay 779b2d283f5b2b28  match

$ recond exceptions show 1
exception #1  open  fee_mismatch  psp_a
detail: fee 27,486.29 IDR, expected 28,275.29 IDR (diff -789.00 IDR)
evidence: { "expected": {...}, "actual": {...}, "ledger": [...], "payments": [...],
  "settlement": [{ ..., "raw": "STL-A-000000322,ORD-20261001-0000545,2026-10-01T23:29:58.124Z,2026-10-03,IDR,1885019.00,27486.29,1857532.71" }] }
audit trail:
  2026-10-20T09:36:18Z  opened      by recond         found by run 1
  2026-10-20T09:36:18Z  resolved    by demo.operator  provider confirmed fee correction in next cycle
```

The 9 `corrupt_reference` faults are not discrepancies: they must be *matched* (by tolerance or after normalisation) to the right transaction, and the scorer checks that they were. Large `|NET DIFF|` on batch classes (`fee_mismatch` 130 ledger txns) is expected: an unitemised payout can only be flagged as a whole.

## What it demonstrates

| Concern | Solution | Where |
|---|---|---|
| Money correctness | `int64` minor units with an explicit ISO 4217 currency and exponent; never floats. The amount parser takes the provider's declared convention (`1.500.000,00` vs `1500000.00`), validates digit grouping and **never rounds**: extra non-zero decimals are an error. Fee math (`bps × gross / 10⁴ + fixed`, four rounding modes) runs in 128 bits. Parse/format round-trips are fuzzed. | `domain/amount.go`, `domain/fee.go` |
| Provider formats | One adapter per format: column lookup by header name, delimiter, date layout, timezone, number convention, item vs payout rows. Row-tolerant, file-strict: a bad row becomes a stored reject, while a file missing required columns is refused. Every adapter has a matching writer used by the generator, so formats are tested in both directions. | `ingest/` |
| Idempotent ingestion | File SHA-256 (re-ingesting is a no-op) **plus** natural row keys (settlement id, ledger id, provider event id). A repeated key with identical content is a duplicate; with different content it is a *conflict*: the first version is kept and the row is rejected with a reason. Bank statements have no row ids, so their key is a content hash plus occurrence index within the file. | `adapters/sqlite/ingest.go`, `ingest/settlement.go` |
| Out-of-order webhooks | A payment's state is a **fold over the set of its events** in provider order (sequence, then provider time, then event id), never in arrival order. An event newer than everything seen applies in O(1). An older one replays that payment's few events. Lifecycle `pending → paid → settled` (`expired` only from pending) and the cumulative refunded amount are both monotone, so a late `pending` cannot overwrite `paid`, and a stale re-send is rejected by the transition rules. Duplicates are dropped by event id. | `domain/payment.go`, `domain/book.go` |
| Webhook security | HMAC-SHA256 over `timestamp.body` (the same scheme ledgerd signs its own webhooks with), compared with `hmac.Equal`, ±5 min timestamp tolerance against replay, multiple signatures accepted for secret rotation, and fail-closed for any provider without a configured secret. Replays inside the window are harmless because event ids are idempotent. | `adapters/httpapi/webhook.go` |
| Matching | Passes, strongest evidence first: **reference** 1:1, **refunded** (nothing due), **tolerance** 1:1 (amount within tolerance and provider time within a window), **batch** N:1, then leftovers past T+N (`missing_in_settlement`) or before it (`awaiting_settlement`). Pure function, sorted iteration only, so the same inputs always give the same outcomes. | `match/engine.go` |
| N:1 payouts | A bank payout covers a window and states a count, but no items. If the window's expected nets sum to the payout, it settled. If it is short, recond searches for the *excluded* subset (O(n) for one, O(n log n) for two, pruned DFS with a node budget beyond). **It refuses to guess on ties**: equal-amount candidates are reported together for review. Algorithm and limits are in [Design notes](#design-notes). | `match/batch.go` |
| Evidence | Each discrepancy stores both sides' records (settlement rows **verbatim**), the provider's payment state, expected vs actual gross/fee/net, the deadline, and a human-readable reason. | `app/run.go` |
| Reproducible runs | A run records its input watermarks (file id, event sequence), the exact rules JSON, engine version, an input digest and a result digest. `recond verify` replays it from the event log with the stored rules and compares digests. Inputs and runs are append-only, enforced by SQLite triggers. | `app/run.go`, `adapters/sqlite/migrations/` |
| Exception queue | One exception per distinct discrepancy (class plus exact records) across runs. Humans resolve or reopen with a mandatory reason; recond auto-closes what disappears and reopens what comes back, but never overrides a human resolution. The audit trail is append-only (triggers reject `UPDATE`/`DELETE`). Over HTTP the resolver is the authenticated principal, never a request field. | `adapters/sqlite/exceptions.go`, `app/exceptions.go` |
| Ground truth | `cmd/gen` builds a world with 14 kinds of injected settlement and ledger faults (dropped on either side, duplicated, amount or fee changed, settled past T+N, reference corrupted, and the batch-level equivalents: an item left out of a payout, a payout item missing from the ledger, a payout credited twice). It adds webhook chaos (duplicated, shuffled, stale re-sends) and payments placed milliseconds before cut-offs, and records the right answer. The property test (24 seeds × 3,000 payments, about 5,000 faults per run) demands **every fault classified exactly, nothing clean flagged, every match paired with the line that settled it**. | `gen/`, `gen/groundtruth_test.go` |
| Testing | Table tests for parsers, fees, the state machine and every matcher path; order-independence property test (3,000 random histories × 8 delivery orders); Go native fuzzing of every parser; end-to-end tests through SQLite and the CSV report; HTTP tests for signature, replay and auth; race detector on. Mutation-checked (see design notes). | `*_test.go` |
| Operability | CLI plus `net/http` API (Go 1.22 routing), RFC 9457 problem+json, `slog`, Prometheus RED + reconciliation metrics, pprof only on a separate admin listener, listeners on `127.0.0.1` by default, graceful shutdown. Reports as `items.csv` (spreadsheet-formula-safe) and `summary.json`. | `cmd/recond`, `adapters/httpapi/` |
| Delivery | Pure Go (`modernc.org/sqlite`, no cgo), distroless non-root image, CI: vet, gofmt, race tests, fuzzing, the scored demo, golangci-lint, image build. | root |

Dependencies: `modernc.org/sqlite`, `prometheus/client_golang`, `x/sync` (errgroup). No router, no ORM, no CLI framework.

## Run it

### CLI

```bash
go build -o bin/recond ./cmd/recond && go build -o bin/gen ./cmd/gen
export RECOND_DB=recond.db

bin/gen -out data -n 10000 -seed 1                  # synthetic world + data/truth.json
bin/recond ingest -dir data                         # or: -kind ledger|settlement|webhooks [-provider p] FILE...
bin/recond run -as-of 2026-10-14                    # gen prints the as-of to use; versioned run + summary
bin/recond report -out reports                      # reports/run-1/items.csv + summary.json
bin/gen score -truth data/truth.json -items reports/run-1/items.csv
bin/recond verify 1                                 # replay from the event log, compare digests
bin/recond exceptions -class amount_mismatch        # the queue (keyset-paginated with -after)
bin/recond exceptions show 17                       # evidence + audit trail
bin/recond resolve 17 -by ops.reviewer -reason "provider confirmed correction in next cycle"
bin/recond payment psp_a ORD-20261001-0000042       # webhook-derived state and event history
bin/recond config                                   # provider rules in effect (RECOND_PROVIDERS_FILE to override)
```

### HTTP

```bash
export RECOND_API_KEYS="$(openssl rand -hex 16):ops"
export RECOND_WEBHOOK_SECRET_PSP_A="$(openssl rand -hex 32)"
bin/recond serve                                    # 127.0.0.1:8080; RECOND_ADMIN_ADDR enables pprof
```

```bash
# a provider delivers a signed webhook (any order, any number of times)
body='{"event_id":"evt_demo_1","payment_ref":"ORD-1","status":"paid","amount":15000000,"currency":"IDR","sequence":2,"occurred_at":"2026-10-01T03:15:22Z"}'
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$RECOND_WEBHOOK_SECRET_PSP_A" | sed 's/^.* //')
curl -s localhost:8080/v1/webhooks/psp_a -H "Webhook-Timestamp: $ts" -H "Webhook-Signature: v1=$sig" -d "$body"
# {"duplicate":false,"out_of_order":false,"verdict":"applied","status_after":"paid"}
```

```
POST /v1/webhooks/{provider}            HMAC-signed provider events
GET  /v1/payments/{provider}/{ref}      state + events                      (X-API-Key)
POST /v1/runs                           run now, optional {"as_of": ...}    (X-API-Key)
GET  /v1/runs  /v1/runs/{id|latest}     summaries
GET  /v1/runs/{id}/items.csv            the report
POST /v1/runs/{id}/verify               replay and compare digests
GET  /v1/exceptions?status=&class=&provider=&after=&limit=
GET  /v1/exceptions/{id}                evidence + audit trail
POST /v1/exceptions/{id}/resolve        {"reason": "..."}; actor = API principal
POST /v1/exceptions/{id}/reopen
GET  /healthz  /readyz  /metrics
```

Configuration is environment-only (see `.env.example`, which ships with empty values). Provider rules (timezone, T+N, tolerances, fee rules per currency) are JSON; `recond config` prints the built-in example rules.

## Develop

```bash
make test        # all tests, incl. the ground-truth property test
make test-race   # same under the race detector (what CI runs)
make lint        # golangci-lint v2 (.golangci.yml)
make fuzz        # short fuzzing pass over every parser (FUZZTIME=20s)
make bench       # micro-benchmarks: parsers, fold, matcher
make bench-e2e SIZES="100000 1000000"
make demo
```

## Design notes

**Payment state is a fold over a set of events, not a sequence.** Providers deliver at least once and in any order, so the state of a payment must not depend on arrival order. recond applies a payment's events in *provider* order (sequence, then provider timestamp, then event id) through monotone transition rules, which makes the state a function of the set of events received. An event newer than everything seen applies in O(1). A late one replays that payment's handful of events, so a late `pending` cannot overwrite `paid`. I first sketched this as a merge of states without history (a CRDT-style join). It stops being associative once a provider sends contradictory events (`paid` and `expired`), and then arrival order leaks back into the result. Keeping per-payment history costs a few rows per payment and removes the problem. A property test checks that 3,000 random histories, including contradictory ones, fold to the same state under 8 random delivery orders with duplicates.

**The provider's clock decides windows and deadlines, not ours.** The ledger posts after the provider: milliseconds usually, minutes when a webhook is retried. If ledger time decides which payout window or T+N date a transaction belongs to, anything near a cut-off is misattributed. recond uses the provider's `paid_at` from the webhook stream when it has it, an itemised line's own transaction time for that line's deadline, and ledger time only as a fallback. The generator deliberately places payments a few hundred milliseconds before cut-offs. Switching the engine back to ledger time made the ground-truth property fail on every seed (transactions moved to the neighbouring payout window, producing spurious exclusions, residuals and mispairings).

**Mutation testing found a hole in the test, not the code.** I injected five deliberate bugs: ledger time instead of provider time, arrival order instead of provider order, refunds ignored in expected amounts, no fee tolerance, and no lifecycle-regression guard. Four failed the ground-truth test immediately. The fifth survived. Ordinary late deliveries never reach the regression guard, because version ordering handles them first, and the generator never produced the case the guard exists for: a provider without sequence numbers re-sending a stale `pending` with a fresh event id and a *later* timestamp. The generator now does that (`StaleResendRate`), and the mutant fails. The same approach later caught a missing cache write-back in chunked webhook ingestion (`chunk_test.go`).

**N:1 payouts: the algorithm and its limits.** Candidates are the transactions still unmatched after the 1:1 passes whose provider time falls in the payout's window; each has an expected net (gross − refunds − fee).
- Sum equals the payout and counts agree: the whole window settled, O(n). This is the case the design optimises for.
- The payout is short by Δ and its count says *m* items are missing: search for the *m*-subset whose nets sum to Δ. That is O(n) for m=1, O(n log n) for m=2 (sorted scan plus binary search), and a pruned DFS with a node budget for m≥3, capped at `max_batch_exclusions` (3).
- Several subsets fit (equal amounts): **no guessing**. Transactions outside every solution are matched; the tied ones become one `missing_in_settlement` group with the payout as context.
- The payout is larger and its count says *k* more items than the ledger has: the candidates are matched, and the residual becomes `missing_in_ledger`.
- Counts agree but money does not: `amount_mismatch`, or `fee_mismatch` when gross agrees and only the charges differ.

Limits: it assumes one payout per provider window (a window split across payouts is a partition problem). Amounts must agree exactly, up to `batch_tolerance_minor`. Without an item count, only up to 3 exclusions are tried, and a surplus cannot be attributed. A batch-level discrepancy flags every transaction in the batch, because unitemised data cannot say which one is wrong. Real amounts tie often, so the ambiguous path will fire in production, and it is meant to.

**"Exact" is a claim about identifiable faults.** The generator refuses to inject a fault no reconciler could attribute, such as dropping one of two equal-net items from a payout, or mangling a reference while a second same-amount candidate sits inside the match window. That is what lets the property test demand exact classification instead of a precision score, and it is also the limit of the claim. The tied cases are covered by unit tests asserting that recond reports ambiguity rather than picks. Each transaction carries at most one fault; compound cases (late *and* wrong amount) are classified by precedence (amount > fee > late) and unit-tested, not property-tested.

**Runs are reproducible because nothing they read can change.** Ledger rows, settlement lines, webhook events, runs and their outcomes are append-only, enforced by SQLite triggers. A correction arrives as new data and is reconciled by a new run. A run stores its input watermarks, the exact rules JSON, the engine version and two digests. The engine is a pure function that iterates only sorted slices (same digest under shuffled input, tested). `recond verify` re-folds payment states from the event log rather than reading the `payments` projection, so a passing verify also cross-checks the projection.

**Exceptions have identity.** An exception is a class plus an exact set of records, fingerprinted, so it persists across runs instead of being re-raised every morning. Its status is a projection of an append-only event log: `opened`, `resolved`, `reopened`, `auto_closed`. recond auto-closes an exception that disappears (the late settlement arrived) and reopens one that comes back, but never overrides a human resolution. A resolution requires a reason, and over HTTP the resolver is the authenticated API principal, not a field in the request.

**SQLite, and a negative result.** Pure-Go SQLite keeps recond a single static binary with real transactions, and lets tests run the real store in a temp directory. Ingestion uses single-row prepared `INSERT … ON CONFLICT DO NOTHING` inside one transaction per file. To cut per-statement overhead I tried multi-row `INSERT … RETURNING`. It was slower at every batch size tried: the 100k webhook backfill went from about 13s to 20–26s at 8–128 rows per statement. The profile showed three causes: the driver binds parameters with a linear scan per parameter (quadratic per statement), `RETURNING` buffers result rows, and multi-row conflict handling needs a statement journal. I reverted it. For plain appends of run results, multi-row does win (save phase 2.1s vs 3.0s single-row at 100k), so it is used there. In-file duplicate webhook deliveries are classified from memory, and new-payment replays fold in memory, which got the 100k backfill from 13.7s to 11–12.5s. Webhook application is chunked (100k events) so a multi-million-event backfill does not hold every event until commit.

**What I'd add next.** Windowed runs with a lookback, instead of reconciling all history every time. The third leg: paid at the provider but absent from the ledger *before* settlement exists. Refunds after settlement (negative lines), and adjustment rows. A Postgres adapter for multi-replica deployments. Per-merchant payouts. Streaming input loading for 10M+ transaction runs.

## Benchmarks

Measured with `scripts/bench.sh 100000 1000000`. Each stage runs as its own process on a fresh database, timed by `getrusage` (`RECOND_STATS=1`). **Environment:** a 4 vCPU cloud VM (Intel Xeon @ 2.80GHz) with 16 GiB RAM, shared with other workloads; Linux, Go 1.24.7, `modernc.org/sqlite` v1.46.1 (SQLite 3.51.2), WAL mode, 256 MiB page cache. These are single runs, so treat them as indicative. The run stage at 1M measured 47.1s, 49.0s and 50.9s across three runs.

| | 100k payment attempts | 1M payment attempts |
|---|---:|---:|
| ledger txns / settlement lines / webhook deliveries | 94,756 / 56,124 / 345,452 | 947,445 / 563,726 / 3,455,492 |
| injected faults → classified exactly | 1,641 → 1,641 | 16,817 → 16,817 |
| **ingest** all files (wall · peak RSS) | 15.1s · 425 MiB | 3m 22.6s · 1.95 GiB |
| **run** load + match + evidence + save | 3.5s · 368 MiB | 50.9s · 2.48 GiB |
| of which matching (engine only) | 0.36s | 8.1s |
| **report** (`items.csv`, one row per record per outcome) | 2.0s · 124 MiB | 22.0s · 516 MiB |
| **verify** (re-fold events, replay, compare digests) | 2.9s · 369 MiB | 36.1s · 2.20 GiB |
| database size | 133 MB | 1.4 GB |

Throughput at 1M: ingest about 24.5k rows/s across all files, dominated by the 3.46M-event webhook backfill. A run reconciles about 18.6k ledger transactions/s end to end; the matcher alone does about 118k/s. Scaling from 100k to 1M is worse than linear (about 14.7x for the run stage). A CPU profile of a 1M run attributes 43% to SQLite inserting the stored result (1M outcome rows, 1.5M member rows), 30% to scanning about 2.5M input rows through `database/sql`, about 10% to GC on a heap of about 2.5 GiB, and about 10% to matching. The engine is not the bottleneck; storage of results and loading of inputs are.

Micro-benchmarks (`make bench`, same machine):

| Benchmark | Result |
|---|---|
| `ParseAmount("1.500.000,00")` | 37 ns/op, 0 allocs |
| fee rule (128-bit) | 5 ns/op |
| `Fold` of 5 shuffled events incl. a duplicate | 1.07 µs/op |
| psp_a / bank_b adapter parse | 310k / 280k rows/s |
| webhook body decode + validate | 3.2 µs/op |
| `match.Reconcile`, in memory, 10k / 100k attempts | 399k / 331k ledger txns/s |

## Layout

```
cmd/recond/              CLI + composition root: ingest, run, verify, report, exceptions, resolve, serve
cmd/gen/                 synthetic world generator with fault injection; `gen score` checks a report against truth
internal/domain/         money, amount parsing, civil dates, fee rules, records, payment state machine, outcome classes
internal/ingest/         ledger / psp_a / bank_b / webhook adapters and matching writers
internal/match/          the engine: passes, N:1 subset search, rules, result digest
internal/gen/            world generator, fault injection, ground truth, scorer, property test
internal/app/            use cases: ingest, run, verify, report, exceptions, webhooks
internal/adapters/
  sqlite/                store, embedded migrations (append-only triggers), ingestion, runs, exception queue
  httpapi/               handlers, HMAC webhook verification, middleware, metrics
internal/config/         env config + example provider rules (embedded)
internal/platform/clock/ testable time
scripts/                 demo.sh, bench.sh
```

## Status

Working end to end: ingestion with idempotency and conflict detection, the state machine, all matching passes including N:1, all outcome classes with evidence, versioned and replayable runs, the exception queue with an audit trail, the CLI, the HTTP API with signed webhooks, reports, the generator, and the scorer. Checked: `go test -race ./...`, `go vet ./...`, `gofmt -l .`, golangci-lint v2 against `.golangci.yml` (0 issues), `make demo`, 60s fuzzing runs of each of the six parser fuzz targets (1.2M–3.2M executions each, no failures), and the benchmarks above.

Not done, or deliberately out of scope:
- **The Dockerfile and CI workflow have not run yet.** The image build needs a Docker daemon, and CI runs on the first push.
- **Runs reconcile everything ingested**, with no date windowing or archival. That suits daily batches of the size measured above, not years of history in one database.
- **The pre-settlement leg isn't classified.** A webhook saying "paid" with no ledger transaction appears as evidence, not as its own class.
- **Not modelled:** refunds after settlement, chargebacks and adjustment rows (negative or zero rows are rejected with a reason); per-merchant payouts (one payout per provider window is assumed); tiered or capped fee schedules; FX (a currency mismatch is an `amount_mismatch`). In the fictional fee model, fees on partially refunded payments are computed on the gross net of refunds.
- **Tolerance matching is greedy** (closest provider time first, deterministic tie-break), not a global assignment.
- **Single SQLite file:** writes are serialised in-process and reads use WAL snapshots. Several replicas would need a Postgres adapter behind the same app layer.
- **Settlement files are ingested through the CLI only;** there is no upload endpoint. Webhook secrets are one per provider: senders can rotate (several signatures are accepted), but a receiver-side dual-secret window is not implemented.
- **Memory:** a run holds every input record, payment state and outcome in memory, peaking at 2.48 GiB at 1M. A webhook backfill holds the file's event keys and the touched payment states until commit, peaking at 1.95 GiB for 3.46M deliveries. Streaming or windowed runs are the fix.
