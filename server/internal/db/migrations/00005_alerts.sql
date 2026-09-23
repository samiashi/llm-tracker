-- +goose Up

-- Alerts that have fired.
--
-- Stored rather than recomputed and sent each time, because the point of an
-- alert is to be noticed once. A rule that re-notifies every evaluation gets
-- muted within a day and then never noticed again.
CREATE TABLE alert (
  id        TEXT PRIMARY KEY,   -- rule + subject, so one subject fires once
  rule      TEXT NOT NULL,
  severity  TEXT NOT NULL DEFAULT 'warning',
  subject   TEXT NOT NULL DEFAULT '',
  title     TEXT NOT NULL,
  detail    TEXT NOT NULL DEFAULT '',
  value     REAL NOT NULL DEFAULT 0,
  fired_at  INTEGER NOT NULL,
  notified  INTEGER NOT NULL DEFAULT 0,
  resolved  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_alert_open ON alert(resolved, fired_at DESC);

-- +goose Down
DROP TABLE alert;
