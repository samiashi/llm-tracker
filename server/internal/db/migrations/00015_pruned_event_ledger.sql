-- +goose Up

-- Every event id Prune has rolled up. A rollup keeps sums, not ids, so this
-- tells a re-sent event (already counted: a no-op) from a late one (not
-- counted: stored beside the rollup). Not a guess from the day and machine:
-- that either lowers a day or counts it twice.
CREATE TABLE pruned_event (id TEXT PRIMARY KEY) WITHOUT ROWID;

-- Days rolled up before the ledger existed have no ids in it, so a re-send
-- cannot be told from a new event there, and ingest refuses them whether or
-- not the server still prunes. Only days that still have rollup rows need
-- it: a retention floor whose rollups are gone protects nothing, and would
-- refuse every re-read of those days for good.
INSERT INTO setting (key, value)
SELECT 'legacy_rollup_floor', date(MAX(day), '+1 day') FROM daily_rollup
HAVING MAX(day) IS NOT NULL;

-- The collector version that read each event, so a corrected adapter's
-- reading replaces an older one's instead of only filling its blanks.
ALTER TABLE event ADD COLUMN collector INTEGER NOT NULL DEFAULT 0;

-- Agents and SourceHealth group all of history on every dashboard poll;
-- without these, each sorts the whole table through a temp B-tree.
CREATE INDEX IF NOT EXISTS idx_event_machine_account ON event(machine_id, account_ref);
CREATE INDEX IF NOT EXISTS idx_event_source_machine_ts ON event(source, machine_id, ts);

-- In no query plan, and each is written on every insert. idx_event_day_machine
-- only repeats idx_event_day for the day scans.
DROP INDEX IF EXISTS idx_event_day_machine;
DROP INDEX IF EXISTS idx_event_effort;
DROP INDEX IF EXISTS idx_event_session;
DROP INDEX IF EXISTS idx_quota_ts;

-- +goose Down
CREATE INDEX IF NOT EXISTS idx_quota_ts ON quota_sample(ts);
CREATE INDEX IF NOT EXISTS idx_event_session ON event(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_event_effort ON event(effort, day);
CREATE INDEX IF NOT EXISTS idx_event_day_machine ON event(day, machine_id);
DROP INDEX IF EXISTS idx_event_source_machine_ts;
DROP INDEX IF EXISTS idx_event_machine_account;
ALTER TABLE event DROP COLUMN collector;
DELETE FROM setting WHERE key = 'legacy_rollup_floor';
DROP TABLE IF EXISTS pruned_event;
