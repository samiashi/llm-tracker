-- +goose Up

-- Day-level rollups, so raw events can be pruned without losing history.
--
-- Raw rows run about 670 bytes each; ten developers is roughly 700MB a year,
-- which is survivable but pointless to keep forever. Rolling up to the
-- dimensions the dashboard actually groups by collapses that by two or three
-- orders of magnitude while keeping every aggregate exact.
--
-- What a rollup cannot answer is anything below day level: the hour-by-hour
-- heatmap and the per-session table read raw events and are therefore limited
-- to the retention window. That is a deliberate trade -- those are operational
-- views about recent activity, not historical accounting.
CREATE TABLE daily_rollup (
  day          TEXT NOT NULL,
  account_ref  TEXT NOT NULL DEFAULT '',
  source       TEXT NOT NULL DEFAULT '',
  surface      TEXT NOT NULL DEFAULT '',
  provider     TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  endpoint     TEXT NOT NULL DEFAULT '',
  effort       TEXT NOT NULL DEFAULT '',
  speed        TEXT NOT NULL DEFAULT '',
  is_subagent  INTEGER NOT NULL DEFAULT 0,
  cost_basis   TEXT NOT NULL DEFAULT 'unknown',
  cost_source  TEXT NOT NULL DEFAULT 'unpriced',

  events            INTEGER NOT NULL DEFAULT 0,
  input_tokens      INTEGER NOT NULL DEFAULT 0,
  output_tokens     INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_5m    INTEGER NOT NULL DEFAULT 0,
  cache_write_1h    INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  web_search_calls  INTEGER NOT NULL DEFAULT 0,
  web_fetch_calls   INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL    NOT NULL DEFAULT 0,

  PRIMARY KEY (day, account_ref, source, surface, provider, model, endpoint,
               effort, speed, is_subagent, cost_basis, cost_source)
);
CREATE INDEX idx_rollup_day ON daily_rollup(day);

-- Aggregate queries read this rather than either table directly, so pruning is
-- invisible to them. A day exists in exactly one side: rollups are written only
-- for days whose raw rows are about to be deleted, and ingest refuses events
-- older than the recorded retention floor.
--
-- Superseded by 00008, which removes the inner GROUP BY. Left as written so
-- the migration history still replays.
-- +goose StatementBegin
CREATE VIEW event_daily AS
  SELECT day, account_ref, source, surface, provider, model, endpoint,
         effort, speed, is_subagent, cost_basis, cost_source,
         COUNT(*)                      AS events,
         SUM(input_tokens)             AS input_tokens,
         SUM(output_tokens)            AS output_tokens,
         SUM(cache_read_tokens)        AS cache_read_tokens,
         SUM(cache_write_5m)           AS cache_write_5m,
         SUM(cache_write_1h)           AS cache_write_1h,
         SUM(reasoning_tokens)         AS reasoning_tokens,
         SUM(web_search_calls)         AS web_search_calls,
         SUM(web_fetch_calls)          AS web_fetch_calls,
         SUM(total_tokens)             AS total_tokens,
         SUM(cost_usd)                 AS cost_usd
  FROM event
  GROUP BY day, account_ref, source, surface, provider, model, endpoint,
           effort, speed, is_subagent, cost_basis, cost_source
  UNION ALL
  SELECT day, account_ref, source, surface, provider, model, endpoint,
         effort, speed, is_subagent, cost_basis, cost_source,
         events, input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM daily_rollup;
-- +goose StatementEnd

-- +goose Down
DROP VIEW IF EXISTS event_daily;
DROP TABLE daily_rollup;
