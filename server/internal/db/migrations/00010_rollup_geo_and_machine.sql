-- +goose Up

-- Put inference_geo and machine_id into daily_rollup's key, and carry
-- machine_id through event_daily.
--
-- machine_id was never in the rollup at all, so the agents table had to read
-- raw events to count a machine's activity -- and that count therefore
-- collapsed to zero the first time retention ran, rendering a healthy,
-- actively-syncing laptop as a nameless row with no events. The table exists
-- to tell "this person was not working" apart from "this person's collector
-- is down", and after a prune it said the second about everybody.
--
-- Existing rollup rows predate the column and cannot be attributed, so they
-- carry an empty machine_id. They keep counting toward every total; they
-- simply do not appear under a specific machine.
--
-- 00007 added the column with ALTER TABLE, which SQLite cannot use to extend a
-- primary key, so the table stayed unique on twelve dimensions while Prune's
-- merge grouped by thirteen. Two events differing only in inference_geo
-- therefore produced two rows for one key and the INSERT aborted with a
-- UNIQUE constraint failure naming twelve columns -- none of them the one at
-- fault.
--
-- Because the whole prune runs in one transaction, the abort rolled back the
-- rollup, the delete and the floor together. Retention did not merely skip
-- that day: it failed identically every night afterwards, the database never
-- shrank, and the error said nothing useful. A day mixing US-pinned and
-- unpinned events is the normal state on any machine using Anthropic, so this
-- fires for most fleets on the first prune.
--
-- Rebuilt rather than altered because SQLite has no ADD CONSTRAINT. The SELECT
-- re-groups on the way across: a table written under the old key may already
-- hold rows that collide under the new one only in the other direction (same
-- thirteen values arriving from two old rows is impossible, but summing is
-- correct either way and costs nothing).

CREATE TABLE daily_rollup_new (
  day           TEXT NOT NULL,
  machine_id    TEXT NOT NULL DEFAULT '',
  account_ref   TEXT NOT NULL DEFAULT '',
  source        TEXT NOT NULL DEFAULT '',
  surface       TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  endpoint      TEXT NOT NULL DEFAULT '',
  effort        TEXT NOT NULL DEFAULT '',
  speed         TEXT NOT NULL DEFAULT '',
  inference_geo TEXT NOT NULL DEFAULT '',
  is_subagent   INTEGER NOT NULL DEFAULT 0,
  cost_basis    TEXT NOT NULL DEFAULT 'unknown',
  cost_source   TEXT NOT NULL DEFAULT 'unpriced',

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

  PRIMARY KEY (day, machine_id, account_ref, source, surface, provider, model,
               endpoint, effort, speed, inference_geo, is_subagent,
               cost_basis, cost_source)
);

INSERT INTO daily_rollup_new
  SELECT day, '', account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         SUM(events), SUM(input_tokens), SUM(output_tokens),
         SUM(cache_read_tokens), SUM(cache_write_5m), SUM(cache_write_1h),
         SUM(reasoning_tokens), SUM(web_search_calls), SUM(web_fetch_calls),
         SUM(total_tokens), SUM(cost_usd)
  FROM daily_rollup
  GROUP BY day, account_ref, source, surface, provider, model, endpoint,
           effort, speed, inference_geo, is_subagent, cost_basis, cost_source;

-- event_daily selects from daily_rollup, so it has to go before the table it
-- reads can be dropped, and come back afterwards.
DROP VIEW IF EXISTS event_daily;
DROP TABLE daily_rollup;
ALTER TABLE daily_rollup_new RENAME TO daily_rollup;
CREATE INDEX idx_rollup_day ON daily_rollup(day);

-- +goose StatementBegin
CREATE VIEW event_daily AS
  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         1 AS events,
         input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM event
  UNION ALL
  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         events, input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM daily_rollup;
-- +goose StatementEnd

-- +goose Down
-- Restores the state 00008 left: no machine_id in the rollup or the view.

-- Restore the twelve-column key, folding the geo dimension back together so
-- the rows still fit it.
CREATE TABLE daily_rollup_old (
  day           TEXT NOT NULL,
  account_ref   TEXT NOT NULL DEFAULT '',
  source        TEXT NOT NULL DEFAULT '',
  surface       TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  endpoint      TEXT NOT NULL DEFAULT '',
  effort        TEXT NOT NULL DEFAULT '',
  speed         TEXT NOT NULL DEFAULT '',
  is_subagent   INTEGER NOT NULL DEFAULT 0,
  cost_basis    TEXT NOT NULL DEFAULT 'unknown',
  cost_source   TEXT NOT NULL DEFAULT 'unpriced',

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

  inference_geo TEXT NOT NULL DEFAULT '',

  PRIMARY KEY (day, account_ref, source, surface, provider, model, endpoint,
               effort, speed, is_subagent, cost_basis, cost_source)
);

INSERT INTO daily_rollup_old
  SELECT day, account_ref, source, surface, provider, model, endpoint,
         effort, speed, is_subagent, cost_basis, cost_source,
         SUM(events), SUM(input_tokens), SUM(output_tokens),
         SUM(cache_read_tokens), SUM(cache_write_5m), SUM(cache_write_1h),
         SUM(reasoning_tokens), SUM(web_search_calls), SUM(web_fetch_calls),
         SUM(total_tokens), SUM(cost_usd),
         MIN(inference_geo)
  FROM daily_rollup
  GROUP BY day, account_ref, source, surface, provider, model, endpoint,
           effort, speed, is_subagent, cost_basis, cost_source;

DROP VIEW IF EXISTS event_daily;
DROP TABLE daily_rollup;
ALTER TABLE daily_rollup_old RENAME TO daily_rollup;
CREATE INDEX idx_rollup_day ON daily_rollup(day);

-- +goose StatementBegin
CREATE VIEW event_daily AS
  SELECT day, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         1 AS events,
         input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM event
  UNION ALL
  SELECT day, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         events, input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM daily_rollup;
-- +goose StatementEnd
