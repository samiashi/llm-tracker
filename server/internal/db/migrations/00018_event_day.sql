-- +goose Up

-- A day-level summary of the live events, kept by triggers on event, which
-- event_daily unions with daily_rollup in place of reading event row by row:
-- re-summing every raw event on every poll grows with the team and its
-- history, and a year of one Mac's events is under a thousand rows here. The
-- view still hands every row over ungrouped (see 00008).
--
-- The same columns and key as daily_rollup (TestRollupColumnsMatchTheSchema),
-- so cost_basis stays in the key and no row sums two bases (invariant 3).
CREATE TABLE event_day (
  day           TEXT NOT NULL,
  machine_id    TEXT NOT NULL,
  account_ref   TEXT NOT NULL,
  source        TEXT NOT NULL,
  surface       TEXT NOT NULL,
  provider      TEXT NOT NULL,
  model         TEXT NOT NULL,
  endpoint      TEXT NOT NULL,
  effort        TEXT NOT NULL,
  speed         TEXT NOT NULL,
  inference_geo TEXT NOT NULL,
  is_subagent   INTEGER NOT NULL,
  cost_basis    TEXT NOT NULL,
  cost_source   TEXT NOT NULL,

  events            INTEGER NOT NULL,
  input_tokens      INTEGER NOT NULL,
  output_tokens     INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  cache_write_5m    INTEGER NOT NULL,
  cache_write_1h    INTEGER NOT NULL,
  reasoning_tokens  INTEGER NOT NULL,
  web_search_calls  INTEGER NOT NULL,
  web_fetch_calls   INTEGER NOT NULL,
  total_tokens      INTEGER NOT NULL,
  cost_usd          REAL    NOT NULL,

  PRIMARY KEY (day, machine_id, account_ref, source, surface, provider, model,
               endpoint, effort, speed, inference_geo, is_subagent,
               cost_basis, cost_source)
) WITHOUT ROWID;

INSERT INTO event_day
  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens),
         SUM(cache_write_5m), SUM(cache_write_1h), SUM(reasoning_tokens),
         SUM(web_search_calls), SUM(web_fetch_calls), SUM(total_tokens), SUM(cost_usd)
  FROM event
  GROUP BY day, machine_id, account_ref, source, surface, provider, model, endpoint,
           effort, speed, inference_geo, is_subagent, cost_basis, cost_source;

-- Each trigger adds a signed delta, inserting the row when it is missing, and
-- drops the row once it holds nothing. No trigger may assume it runs before
-- another: 00016's delete a row inside the INSERT that stored it, so the
-- delete can reach this table first. An UPDATE would then find no row and do
-- nothing, and a row dropped when only its count reached zero would lose the
-- key's other events. The cost test allows for float residue, not for usage.

-- +goose StatementBegin
CREATE TRIGGER event_day_insert AFTER INSERT ON event BEGIN
  INSERT INTO event_day VALUES (
    NEW.day, NEW.machine_id, NEW.account_ref, NEW.source, NEW.surface,
    NEW.provider, NEW.model, NEW.endpoint, NEW.effort, NEW.speed,
    NEW.inference_geo, NEW.is_subagent, NEW.cost_basis, NEW.cost_source,
    1, NEW.input_tokens, NEW.output_tokens, NEW.cache_read_tokens,
    NEW.cache_write_5m, NEW.cache_write_1h, NEW.reasoning_tokens,
    NEW.web_search_calls, NEW.web_fetch_calls, NEW.total_tokens, NEW.cost_usd)
  ON CONFLICT DO UPDATE SET
    events            = events            + excluded.events,
    input_tokens      = input_tokens      + excluded.input_tokens,
    output_tokens     = output_tokens     + excluded.output_tokens,
    cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
    cache_write_5m    = cache_write_5m    + excluded.cache_write_5m,
    cache_write_1h    = cache_write_1h    + excluded.cache_write_1h,
    reasoning_tokens  = reasoning_tokens  + excluded.reasoning_tokens,
    web_search_calls  = web_search_calls  + excluded.web_search_calls,
    web_fetch_calls   = web_fetch_calls   + excluded.web_fetch_calls,
    total_tokens      = total_tokens      + excluded.total_tokens,
    cost_usd          = cost_usd          + excluded.cost_usd;
  DELETE FROM event_day
   WHERE day = NEW.day AND machine_id = NEW.machine_id AND account_ref = NEW.account_ref
     AND source = NEW.source AND surface = NEW.surface AND provider = NEW.provider
     AND model = NEW.model AND endpoint = NEW.endpoint AND effort = NEW.effort
     AND speed = NEW.speed AND inference_geo = NEW.inference_geo AND is_subagent = NEW.is_subagent
     AND cost_basis = NEW.cost_basis AND cost_source = NEW.cost_source
     AND events = 0 AND input_tokens = 0 AND output_tokens = 0
     AND cache_read_tokens = 0 AND cache_write_5m = 0 AND cache_write_1h = 0
     AND reasoning_tokens = 0 AND web_search_calls = 0 AND web_fetch_calls = 0
     AND total_tokens = 0 AND ABS(cost_usd) < 1e-6;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_day_delete AFTER DELETE ON event BEGIN
  INSERT INTO event_day VALUES (
    OLD.day, OLD.machine_id, OLD.account_ref, OLD.source, OLD.surface,
    OLD.provider, OLD.model, OLD.endpoint, OLD.effort, OLD.speed,
    OLD.inference_geo, OLD.is_subagent, OLD.cost_basis, OLD.cost_source,
    -1, -OLD.input_tokens, -OLD.output_tokens, -OLD.cache_read_tokens,
    -OLD.cache_write_5m, -OLD.cache_write_1h, -OLD.reasoning_tokens,
    -OLD.web_search_calls, -OLD.web_fetch_calls, -OLD.total_tokens, -OLD.cost_usd)
  ON CONFLICT DO UPDATE SET
    events            = events            + excluded.events,
    input_tokens      = input_tokens      + excluded.input_tokens,
    output_tokens     = output_tokens     + excluded.output_tokens,
    cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
    cache_write_5m    = cache_write_5m    + excluded.cache_write_5m,
    cache_write_1h    = cache_write_1h    + excluded.cache_write_1h,
    reasoning_tokens  = reasoning_tokens  + excluded.reasoning_tokens,
    web_search_calls  = web_search_calls  + excluded.web_search_calls,
    web_fetch_calls   = web_fetch_calls   + excluded.web_fetch_calls,
    total_tokens      = total_tokens      + excluded.total_tokens,
    cost_usd          = cost_usd          + excluded.cost_usd;
  DELETE FROM event_day
   WHERE day = OLD.day AND machine_id = OLD.machine_id AND account_ref = OLD.account_ref
     AND source = OLD.source AND surface = OLD.surface AND provider = OLD.provider
     AND model = OLD.model AND endpoint = OLD.endpoint AND effort = OLD.effort
     AND speed = OLD.speed AND inference_geo = OLD.inference_geo AND is_subagent = OLD.is_subagent
     AND cost_basis = OLD.cost_basis AND cost_source = OLD.cost_source
     AND events = 0 AND input_tokens = 0 AND output_tokens = 0
     AND cache_read_tokens = 0 AND cache_write_5m = 0 AND cache_write_1h = 0
     AND reasoning_tokens = 0 AND web_search_calls = 0 AND web_fetch_calls = 0
     AND total_tokens = 0 AND ABS(cost_usd) < 1e-6;
END;
-- +goose StatementEnd

-- An update is the old row's delete and the new row's insert: a merge by a
-- newer collector can move an event to another day, model or effort, and a
-- reprice to another cost_source.
-- +goose StatementBegin
CREATE TRIGGER event_day_update AFTER UPDATE ON event BEGIN
  INSERT INTO event_day VALUES (
    OLD.day, OLD.machine_id, OLD.account_ref, OLD.source, OLD.surface,
    OLD.provider, OLD.model, OLD.endpoint, OLD.effort, OLD.speed,
    OLD.inference_geo, OLD.is_subagent, OLD.cost_basis, OLD.cost_source,
    -1, -OLD.input_tokens, -OLD.output_tokens, -OLD.cache_read_tokens,
    -OLD.cache_write_5m, -OLD.cache_write_1h, -OLD.reasoning_tokens,
    -OLD.web_search_calls, -OLD.web_fetch_calls, -OLD.total_tokens, -OLD.cost_usd)
  ON CONFLICT DO UPDATE SET
    events            = events            + excluded.events,
    input_tokens      = input_tokens      + excluded.input_tokens,
    output_tokens     = output_tokens     + excluded.output_tokens,
    cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
    cache_write_5m    = cache_write_5m    + excluded.cache_write_5m,
    cache_write_1h    = cache_write_1h    + excluded.cache_write_1h,
    reasoning_tokens  = reasoning_tokens  + excluded.reasoning_tokens,
    web_search_calls  = web_search_calls  + excluded.web_search_calls,
    web_fetch_calls   = web_fetch_calls   + excluded.web_fetch_calls,
    total_tokens      = total_tokens      + excluded.total_tokens,
    cost_usd          = cost_usd          + excluded.cost_usd;
  DELETE FROM event_day
   WHERE day = OLD.day AND machine_id = OLD.machine_id AND account_ref = OLD.account_ref
     AND source = OLD.source AND surface = OLD.surface AND provider = OLD.provider
     AND model = OLD.model AND endpoint = OLD.endpoint AND effort = OLD.effort
     AND speed = OLD.speed AND inference_geo = OLD.inference_geo AND is_subagent = OLD.is_subagent
     AND cost_basis = OLD.cost_basis AND cost_source = OLD.cost_source
     AND events = 0 AND input_tokens = 0 AND output_tokens = 0
     AND cache_read_tokens = 0 AND cache_write_5m = 0 AND cache_write_1h = 0
     AND reasoning_tokens = 0 AND web_search_calls = 0 AND web_fetch_calls = 0
     AND total_tokens = 0 AND ABS(cost_usd) < 1e-6;
  INSERT INTO event_day VALUES (
    NEW.day, NEW.machine_id, NEW.account_ref, NEW.source, NEW.surface,
    NEW.provider, NEW.model, NEW.endpoint, NEW.effort, NEW.speed,
    NEW.inference_geo, NEW.is_subagent, NEW.cost_basis, NEW.cost_source,
    1, NEW.input_tokens, NEW.output_tokens, NEW.cache_read_tokens,
    NEW.cache_write_5m, NEW.cache_write_1h, NEW.reasoning_tokens,
    NEW.web_search_calls, NEW.web_fetch_calls, NEW.total_tokens, NEW.cost_usd)
  ON CONFLICT DO UPDATE SET
    events            = events            + excluded.events,
    input_tokens      = input_tokens      + excluded.input_tokens,
    output_tokens     = output_tokens     + excluded.output_tokens,
    cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
    cache_write_5m    = cache_write_5m    + excluded.cache_write_5m,
    cache_write_1h    = cache_write_1h    + excluded.cache_write_1h,
    reasoning_tokens  = reasoning_tokens  + excluded.reasoning_tokens,
    web_search_calls  = web_search_calls  + excluded.web_search_calls,
    web_fetch_calls   = web_fetch_calls   + excluded.web_fetch_calls,
    total_tokens      = total_tokens      + excluded.total_tokens,
    cost_usd          = cost_usd          + excluded.cost_usd;
  DELETE FROM event_day
   WHERE day = NEW.day AND machine_id = NEW.machine_id AND account_ref = NEW.account_ref
     AND source = NEW.source AND surface = NEW.surface AND provider = NEW.provider
     AND model = NEW.model AND endpoint = NEW.endpoint AND effort = NEW.effort
     AND speed = NEW.speed AND inference_geo = NEW.inference_geo AND is_subagent = NEW.is_subagent
     AND cost_basis = NEW.cost_basis AND cost_source = NEW.cost_source
     AND events = 0 AND input_tokens = 0 AND output_tokens = 0
     AND cache_read_tokens = 0 AND cache_write_5m = 0 AND cache_write_1h = 0
     AND reasoning_tokens = 0 AND web_search_calls = 0 AND web_fetch_calls = 0
     AND total_tokens = 0 AND ABS(cost_usd) < 1e-6;
END;
-- +goose StatementEnd

DROP VIEW IF EXISTS event_daily;

-- +goose StatementBegin
CREATE VIEW event_daily AS
  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         events, input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM event_day
  UNION ALL
  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
         events, input_tokens, output_tokens, cache_read_tokens,
         cache_write_5m, cache_write_1h, reasoning_tokens,
         web_search_calls, web_fetch_calls, total_tokens, cost_usd
  FROM daily_rollup;
-- +goose StatementEnd

-- +goose Down

-- Back to 00010's view, which reads event row by row.
DROP VIEW IF EXISTS event_daily;

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

DROP TRIGGER IF EXISTS event_day_update;
DROP TRIGGER IF EXISTS event_day_delete;
DROP TRIGGER IF EXISTS event_day_insert;
DROP TABLE IF EXISTS event_day;
