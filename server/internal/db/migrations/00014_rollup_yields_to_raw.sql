-- +goose Up

-- Index the day/machine pair that ingest uses to retire a superseded rollup.
--
-- event_daily unions live events with the day-level rollups pruning leaves
-- behind, and nothing stops a day appearing in both. That is why ingest
-- refuses anything below the retention floor, and why a day once collapsed
-- could never come back even though every agent still held it -- 610,101
-- events on this machine, kept locally and permanently refused.
--
-- The obvious repair is to teach the view to prefer raw rows, but the guard
-- has to sit on both of its branches, and the raw branch is every event in the
-- database. A correlated subquery there is paid on every dashboard query
-- forever, to settle a question that matters only while a day is being
-- restored.
--
-- So it is settled once, at write time: when ingest accepts an event for a day
-- and machine that a rollup covers, that rollup is deleted in the same
-- transaction. Exactly one of the two answers for any day, the view stays a
-- plain union, and reads cost nothing.
--
-- Per machine, not per day: the rollup's primary key starts (day, machine_id),
-- so one laptop restoring its history cannot retire another's.
CREATE INDEX IF NOT EXISTS idx_event_day_machine ON event(day, machine_id);

-- +goose Down
DROP INDEX IF EXISTS idx_event_day_machine;
