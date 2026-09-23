-- +goose Up

-- Each index is written on every insert. idx_event_machine_account served only
-- Agents, which now reads event_day. idx_event_model served no query, and led
-- the planner to read a model's whole history for a windowed one.
DROP INDEX IF EXISTS idx_event_machine_account;
DROP INDEX IF EXISTS idx_event_model;

-- +goose Down
CREATE INDEX IF NOT EXISTS idx_event_model ON event(model);
CREATE INDEX IF NOT EXISTS idx_event_machine_account ON event(machine_id, account_ref);
