-- +goose Up

-- Reasoning effort and speed mode, both already present in the transcripts and
-- previously discarded.
--
-- Speed is a pricing input (fast mode doubles Opus rates). Effort is not --
-- reasoning tokens bill as ordinary output -- but it is the main lever anyone
-- has over what a turn costs, so being able to break spend down by it is the
-- point of storing it.
ALTER TABLE event ADD COLUMN speed  TEXT NOT NULL DEFAULT '';
ALTER TABLE event ADD COLUMN effort TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_event_effort ON event(effort, day);

-- +goose Down
DROP INDEX idx_event_effort;
ALTER TABLE event DROP COLUMN effort;
ALTER TABLE event DROP COLUMN speed;
