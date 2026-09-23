-- +goose Up

-- Alerting is gone. Dropping the table rather than leaving it orphaned: a
-- table nothing writes to and nothing reads is just a question for whoever
-- opens this schema next.
DROP TABLE IF EXISTS alert;

-- +goose Down
CREATE TABLE alert (
  id        TEXT PRIMARY KEY,
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
