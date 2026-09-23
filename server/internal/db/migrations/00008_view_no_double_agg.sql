-- +goose Up

-- Rebuild event_daily without the inner GROUP BY.
--
-- The original view pre-aggregated raw events by all twelve dimensions, and
-- every caller then aggregated the result again. SQLite materialised the inner
-- grouping into a temp B-tree first, which at a million events cost 3.2s
-- against 226ms for the same sum over the raw table — a 14x penalty for work
-- the outer query immediately redid. The dashboard simply never appeared.
--
-- Emitting one row per event with a literal 1 for its count is identical for
-- any aggregating caller, and every caller aggregates.
--
-- IF EXISTS because this statement has to survive a down-and-up cycle: the
-- Down below used to leave no view at all, so rolling forward again failed
-- here and the schema could move in neither direction.
DROP VIEW IF EXISTS event_daily;

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

-- +goose Down

-- Restore 00004's view. Dropping without recreating left the database with no
-- event_daily at all: the server still started, and then every aggregate query
-- failed with "no such table" while /healthz reported ok.
DROP VIEW IF EXISTS event_daily;

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
