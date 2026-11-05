-- recond schema v2: windowed reconciliation runs (expand step, additive only).
--
-- See docs/lifecycle/windowed-runs/04-migration-plan.md. Inputs are
-- append-only, so nothing here rewrites a stored row: the new run columns
-- are NULL for every existing run (= full history, which is what those runs
-- were), and a record's window date is derived from columns it already has
-- and indexed through expressions instead of being backfilled.

-- The window a run reconciled. NULL window_from: the whole history.
ALTER TABLE runs ADD COLUMN window_from TEXT;          -- YYYY-MM-DD, inclusive
ALTER TABLE runs ADD COLUMN window_to TEXT;            -- YYYY-MM-DD, inclusive
ALTER TABLE runs ADD COLUMN window_lookbehind INTEGER; -- days
ALTER TABLE runs ADD COLUMN window_lookahead INTEGER;  -- days

-- The window date of an exception's earliest record. A windowed run
-- auto-closes only exceptions dated inside its window; NULL (exceptions not
-- yet seen by a run of this release) is never auto-closed by a windowed run.
ALTER TABLE exceptions ADD COLUMN window_date TEXT;    -- YYYY-MM-DD

-- Window loading: ledger by booking time, lines by provider transaction time
-- or payout period start, lines with neither by settlement date.
CREATE INDEX ledger_txns_created ON ledger_txns (created_at);
CREATE INDEX settlement_lines_window ON settlement_lines (coalesce(txn_at, covers_from));
CREATE INDEX settlement_lines_untimed ON settlement_lines (settled_on)
    WHERE txn_at IS NULL AND covers_from IS NULL;

-- Counterpart lookups for records whose partner lies outside a window. The
-- key is the reference with ASCII case and surrounding spaces normalised
-- (SQLite's upper/trim), matching match.ProbeKey.
CREATE INDEX ledger_txns_ref ON ledger_txns (provider, upper(trim(reference)));
CREATE INDEX settlement_lines_item_ref ON settlement_lines (provider, upper(trim(reference)))
    WHERE kind = 'item';
CREATE INDEX settlement_lines_payout ON settlement_lines (provider, covers_from)
    WHERE kind = 'payout';
