-- recond schema v1.
--
-- Inputs (ledger, settlement lines, webhook events) and run results are
-- append-only: triggers reject UPDATE and DELETE. A correction arrives as new
-- data and is reconciled by a new run; nothing a past run read can change
-- underneath it, which is what makes runs reproducible.

CREATE TABLE ingest_files (
    id             INTEGER PRIMARY KEY,
    kind           TEXT    NOT NULL CHECK (kind IN ('ledger', 'settlement', 'webhooks')),
    source         TEXT    NOT NULL,          -- provider, or 'ledger'
    name           TEXT    NOT NULL,          -- base name only
    sha256         TEXT    NOT NULL UNIQUE,   -- whole-file idempotency
    size_bytes     INTEGER NOT NULL,
    rows_total     INTEGER NOT NULL DEFAULT 0,
    rows_inserted  INTEGER NOT NULL DEFAULT 0,
    rows_duplicate INTEGER NOT NULL DEFAULT 0,
    rows_conflict  INTEGER NOT NULL DEFAULT 0,
    rows_rejected  INTEGER NOT NULL DEFAULT 0,
    ingested_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE ingest_rejects (
    file_id INTEGER NOT NULL REFERENCES ingest_files (id),
    line    INTEGER NOT NULL,
    reason  TEXT    NOT NULL,
    raw     TEXT    NOT NULL
) STRICT;
CREATE INDEX ingest_rejects_file ON ingest_rejects (file_id, line);

CREATE TABLE ledger_txns (
    id           TEXT    PRIMARY KEY,         -- natural key: ledger transfer id
    file_id      INTEGER NOT NULL REFERENCES ingest_files (id),
    provider     TEXT    NOT NULL,
    reference    TEXT    NOT NULL,
    amount       INTEGER NOT NULL CHECK (amount > 0),
    currency     TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,            -- unix ns, UTC
    from_account TEXT    NOT NULL,
    to_account   TEXT    NOT NULL,
    row_hash     TEXT    NOT NULL
) STRICT;

CREATE TABLE settlement_lines (
    id          TEXT    PRIMARY KEY,          -- provider-scoped natural key
    file_id     INTEGER NOT NULL REFERENCES ingest_files (id),
    provider    TEXT    NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('item', 'payout')),
    reference   TEXT    NOT NULL,
    currency    TEXT    NOT NULL,
    gross       INTEGER NOT NULL,
    fee         INTEGER NOT NULL,
    net         INTEGER NOT NULL,
    fee_known   INTEGER NOT NULL,
    txn_at      INTEGER,                      -- unix ns; NULL when not reported
    settled_on  TEXT    NOT NULL,             -- YYYY-MM-DD
    covers_from INTEGER,
    covers_to   INTEGER,
    item_count  INTEGER NOT NULL DEFAULT 0,
    source_line INTEGER NOT NULL,
    raw         TEXT    NOT NULL,
    row_hash    TEXT    NOT NULL,
    CHECK (net = gross - fee)
) STRICT;

CREATE TABLE webhook_events (
    seq             INTEGER PRIMARY KEY,      -- arrival order: the run watermark
    provider        TEXT    NOT NULL,
    event_id        TEXT    NOT NULL,
    payment_ref     TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    amount          INTEGER NOT NULL,
    refunded_amount INTEGER NOT NULL,
    currency        TEXT    NOT NULL,
    provider_seq    INTEGER NOT NULL,
    occurred_at     INTEGER NOT NULL,
    received_at     INTEGER NOT NULL,
    file_id         INTEGER REFERENCES ingest_files (id), -- NULL when received over HTTP
    row_hash        TEXT    NOT NULL,
    UNIQUE (provider, event_id)
) STRICT;
CREATE INDEX webhook_events_payment ON webhook_events (provider, payment_ref, seq);

-- Current state per payment, maintained from webhook_events. A projection:
-- runs re-fold events up to their watermark instead of reading this table.
CREATE TABLE payments (
    provider        TEXT    NOT NULL,
    payment_ref     TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    lifecycle       TEXT    NOT NULL,
    amount          INTEGER NOT NULL,
    currency        TEXT    NOT NULL,
    refunded_amount INTEGER NOT NULL,
    fully_refunded  INTEGER NOT NULL,
    paid_at         INTEGER,
    settled_at      INTEGER,
    last_seq        INTEGER NOT NULL,
    last_at         INTEGER NOT NULL,
    last_event_id   TEXT    NOT NULL,
    events          INTEGER NOT NULL,
    rejected        INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (provider, payment_ref)
) STRICT, WITHOUT ROWID;

CREATE TABLE runs (
    id             INTEGER PRIMARY KEY,
    created_at     INTEGER NOT NULL,
    as_of          INTEGER NOT NULL,
    engine_version TEXT    NOT NULL,
    config_json    TEXT    NOT NULL,          -- the exact rules used; replays use this copy
    config_sha256  TEXT    NOT NULL,
    max_file_id    INTEGER NOT NULL,          -- input watermarks
    max_event_seq  INTEGER NOT NULL,
    input_digest   TEXT    NOT NULL,
    result_digest  TEXT    NOT NULL,
    ledger_count   INTEGER NOT NULL,
    line_count     INTEGER NOT NULL,
    payment_count  INTEGER NOT NULL,
    summary_json   TEXT    NOT NULL,
    duration_ms    INTEGER NOT NULL
) STRICT;

CREATE TABLE run_outcomes (
    run_id         INTEGER NOT NULL REFERENCES runs (id),
    outcome_id     INTEGER NOT NULL,
    class          TEXT    NOT NULL,
    rule           TEXT    NOT NULL,
    provider       TEXT    NOT NULL,
    currency       TEXT    NOT NULL,
    expected_gross INTEGER NOT NULL,
    expected_fee   INTEGER NOT NULL,
    expected_net   INTEGER NOT NULL,
    actual_gross   INTEGER NOT NULL,
    actual_fee     INTEGER NOT NULL,
    actual_net     INTEGER NOT NULL,
    deadline       TEXT    NOT NULL,
    settled_on     TEXT    NOT NULL,
    detail         TEXT    NOT NULL,
    evidence       TEXT,                      -- JSON; discrepancies only
    PRIMARY KEY (run_id, outcome_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX run_outcomes_class ON run_outcomes (run_id, class);

CREATE TABLE run_members (
    run_id     INTEGER NOT NULL,
    outcome_id INTEGER NOT NULL,
    role       TEXT    NOT NULL CHECK (role IN ('ledger', 'line', 'context')),
    entity_id  TEXT    NOT NULL,
    PRIMARY KEY (run_id, outcome_id, role, entity_id)
) STRICT, WITHOUT ROWID;

-- Exception queue. One row per distinct discrepancy (class + records), kept
-- across runs; status is a projection of exception_events, the audit trail.
CREATE TABLE exceptions (
    id          INTEGER PRIMARY KEY,
    fingerprint TEXT    NOT NULL UNIQUE,
    class       TEXT    NOT NULL,
    provider    TEXT    NOT NULL,
    currency    TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('open', 'resolved', 'auto_closed')),
    first_run   INTEGER NOT NULL,
    last_run    INTEGER NOT NULL,
    run_id      INTEGER NOT NULL,             -- latest occurrence, for evidence
    outcome_id  INTEGER NOT NULL,
    refs        TEXT    NOT NULL,
    delta       INTEGER NOT NULL,             -- actual net - expected net
    detail      TEXT    NOT NULL,
    opened_at   INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX exceptions_status ON exceptions (status, class);

CREATE TABLE exception_events (
    id           INTEGER PRIMARY KEY,
    exception_id INTEGER NOT NULL REFERENCES exceptions (id),
    action       TEXT    NOT NULL CHECK (action IN ('opened', 'resolved', 'reopened', 'auto_closed')),
    actor        TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    run_id       INTEGER,
    at           INTEGER NOT NULL
) STRICT;
CREATE INDEX exception_events_exception ON exception_events (exception_id, id);

CREATE TRIGGER exception_events_append_only_u BEFORE UPDATE ON exception_events
BEGIN SELECT RAISE(ABORT, 'exception_events is append-only'); END;
CREATE TRIGGER exception_events_append_only_d BEFORE DELETE ON exception_events
BEGIN SELECT RAISE(ABORT, 'exception_events is append-only'); END;

CREATE TRIGGER ledger_txns_immutable_u BEFORE UPDATE ON ledger_txns
BEGIN SELECT RAISE(ABORT, 'ledger_txns is append-only'); END;
CREATE TRIGGER ledger_txns_immutable_d BEFORE DELETE ON ledger_txns
BEGIN SELECT RAISE(ABORT, 'ledger_txns is append-only'); END;
CREATE TRIGGER settlement_lines_immutable_u BEFORE UPDATE ON settlement_lines
BEGIN SELECT RAISE(ABORT, 'settlement_lines is append-only'); END;
CREATE TRIGGER settlement_lines_immutable_d BEFORE DELETE ON settlement_lines
BEGIN SELECT RAISE(ABORT, 'settlement_lines is append-only'); END;
CREATE TRIGGER webhook_events_immutable_u BEFORE UPDATE ON webhook_events
BEGIN SELECT RAISE(ABORT, 'webhook_events is append-only'); END;
CREATE TRIGGER webhook_events_immutable_d BEFORE DELETE ON webhook_events
BEGIN SELECT RAISE(ABORT, 'webhook_events is append-only'); END;
CREATE TRIGGER runs_immutable_u BEFORE UPDATE ON runs
BEGIN SELECT RAISE(ABORT, 'runs are immutable'); END;
CREATE TRIGGER runs_immutable_d BEFORE DELETE ON runs
BEGIN SELECT RAISE(ABORT, 'runs are immutable'); END;
CREATE TRIGGER run_outcomes_immutable_u BEFORE UPDATE ON run_outcomes
BEGIN SELECT RAISE(ABORT, 'run_outcomes are immutable'); END;
CREATE TRIGGER run_outcomes_immutable_d BEFORE DELETE ON run_outcomes
BEGIN SELECT RAISE(ABORT, 'run_outcomes are immutable'); END;
CREATE TRIGGER run_members_immutable_u BEFORE UPDATE ON run_members
BEGIN SELECT RAISE(ABORT, 'run_members are immutable'); END;
CREATE TRIGGER run_members_immutable_d BEFORE DELETE ON run_members
BEGIN SELECT RAISE(ABORT, 'run_members are immutable'); END;
