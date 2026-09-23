-- +goose Up

-- Record which allowance pool a quota sample measures.
--
-- A provider runs several concurrently. Codex reports a general "codex" pool
-- beside a per-model one ("codex_bengalfox", named "GPT-5.3-Codex-Spark") and
-- a "premium" pool; they are unrelated, and one sitting at 0% says nothing
-- about another at 91%.
--
-- The collector keyed samples on the JSON slot they arrived in -- "primary"
-- or "secondary" -- plus the hour. Every pool reporting in the same hour
-- therefore competed for one id, and the upsert's MAX(used_percent) kept
-- whichever happened to be highest. The dashboard then showed the winner of
-- that collision as if it were the account's allowance.
--
-- Samples written before this carry an empty limit_id. They are not
-- retrofittable -- a collided row is a blend of pools with no record of which
-- ones -- so the queries shadow them for any account that has attributed
-- samples, rather than deleting a year of history outright. The collector
-- bump re-reads Codex's rollouts, which supplies that attribution wherever
-- the rollouts still exist, and the unattributed rows fall away on their own.
ALTER TABLE quota_sample ADD COLUMN limit_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE quota_sample ADD COLUMN limit_name TEXT NOT NULL DEFAULT '';

-- The allowance queries scan by account and pool over a time range.
CREATE INDEX IF NOT EXISTS idx_quota_pool
  ON quota_sample(account_ref, source, limit_id, window_minutes, ts);

-- +goose Down
DROP INDEX IF EXISTS idx_quota_pool;
ALTER TABLE quota_sample DROP COLUMN limit_name;
ALTER TABLE quota_sample DROP COLUMN limit_id;
