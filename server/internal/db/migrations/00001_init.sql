-- +goose Up

-- One row per model response, deduplicated by the agent's idempotency key.
--
-- The unique primary key is what makes re-syncing safe: an agent that resends
-- a batch after a network failure, or replays its archive onto a fresh server,
-- cannot inflate the totals.
CREATE TABLE event (
  id            TEXT PRIMARY KEY,
  native_id     TEXT NOT NULL DEFAULT '',
  source        TEXT NOT NULL,
  surface       TEXT NOT NULL DEFAULT 'unknown',
  ts            INTEGER NOT NULL,
  day           TEXT NOT NULL,

  machine_id    TEXT NOT NULL,
  account_ref   TEXT NOT NULL DEFAULT '',

  provider      TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  endpoint      TEXT NOT NULL DEFAULT '',

  input_tokens        INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_write_5m      INTEGER NOT NULL DEFAULT 0,
  cache_write_1h      INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
  web_search_calls    INTEGER NOT NULL DEFAULT 0,
  web_fetch_calls     INTEGER NOT NULL DEFAULT 0,
  total_tokens        INTEGER NOT NULL DEFAULT 0,

  -- cost_basis separates metered spend from seat usage. They are never summed:
  -- a subscription token costs nothing at the margin, and adding its rate-card
  -- equivalent to real API spend produces a total that means nothing.
  cost_basis    TEXT NOT NULL DEFAULT 'unknown',
  cost_usd      REAL NOT NULL DEFAULT 0,
  -- 'native'   -> the harness computed it (opencode)
  -- 'table'    -> our price table resolved it
  -- 'unpriced' -> no rate matched; tokens counted, cost deliberately not guessed
  cost_source   TEXT NOT NULL DEFAULT 'unpriced',

  session_id    TEXT NOT NULL DEFAULT '',
  project_path  TEXT NOT NULL DEFAULT '',
  git_branch    TEXT NOT NULL DEFAULT '',
  is_subagent   INTEGER NOT NULL DEFAULT 0,

  received_at   INTEGER NOT NULL
);

CREATE INDEX idx_event_day ON event(day);
CREATE INDEX idx_event_account_day ON event(account_ref, day);
CREATE INDEX idx_event_source_day ON event(source, day);
CREATE INDEX idx_event_model ON event(model);

-- An account is one provider login. A person may own several.
CREATE TABLE account (
  ref        TEXT PRIMARY KEY,
  provider   TEXT NOT NULL DEFAULT '',
  email      TEXT NOT NULL DEFAULT '',
  plan_type  TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL
);

-- Machines report in; several may belong to one person.
CREATE TABLE machine (
  id            TEXT PRIMARY KEY,
  hostname      TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  first_seen    INTEGER NOT NULL,
  last_seen     INTEGER NOT NULL
);

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
  is_overage     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_quota_ts ON quota_sample(ts);

-- Harnesses found on a machine that no adapter covers. This is the backlog,
-- populated by what the team actually runs rather than by guesswork.
CREATE TABLE unknown_source (
  machine_id TEXT NOT NULL,
  path       TEXT NOT NULL,
  hint       TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (machine_id, path)
);

-- +goose Down
DROP TABLE unknown_source;
DROP TABLE quota_sample;
DROP TABLE machine;
DROP TABLE account;
DROP TABLE event;
