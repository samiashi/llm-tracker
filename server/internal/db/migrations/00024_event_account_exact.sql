-- +goose Up

-- Whether an event's account was read from a record of which account served
-- the request, rather than inferred from who was signed in (see upsertEvent).
-- Only an exact account may replace one already stored.
ALTER TABLE event ADD COLUMN account_exact INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE event DROP COLUMN account_exact;
