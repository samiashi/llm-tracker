-- +goose Up

-- Store the harness version each event came from.
--
-- schema.Event has carried agent_version since it was written, two adapters
-- populate it with the real Claude Code / Codex version, and the event INSERT
-- had no such column -- so it was parsed off disk, shipped over the network
-- and dropped on the floor every time. The only agent_version column was on
-- `machine`, which holds the *collector's* version: a different value with
-- the same name.
--
-- It is worth keeping. "Which Claude Code version produced this spend" is
-- unanswerable without it, and a per-harness-version regression -- a release
-- that starts reporting usage differently -- is invisible.
ALTER TABLE event ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE event DROP COLUMN agent_version;
