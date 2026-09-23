-- +goose Up

-- Plan-quota readings were stored for a dashboard view that was never built:
-- nothing ever read quota_sample, and no prune trimmed it, so it only grew.
-- Agents that still send readings are acknowledged as before and the readings
-- dropped unread. Its index goes with it.
DROP TABLE quota_sample;

-- +goose Down
-- As 00012 left it, empty: the readings are gone.
CREATE TABLE quota_sample (
  id             TEXT PRIMARY KEY,
  source         TEXT NOT NULL,
  ts             INTEGER NOT NULL,
  machine_id     TEXT NOT NULL,
  account_ref    TEXT NOT NULL DEFAULT '',
  plan_type      TEXT NOT NULL DEFAULT '',
  window_minutes INTEGER NOT NULL DEFAULT 0,
  used_percent   REAL NOT NULL DEFAULT 0,
  resets_at      INTEGER NOT NULL DEFAULT 0,
  is_overage     INTEGER NOT NULL DEFAULT 0,
  limit_id       TEXT NOT NULL DEFAULT '',
  limit_name     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_quota_pool
  ON quota_sample(account_ref, source, limit_id, window_minutes, ts);
