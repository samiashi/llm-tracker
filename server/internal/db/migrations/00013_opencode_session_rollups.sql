-- +goose Up

-- Remove opencode's session-level rollups, which the collector has replaced
-- with one event per response.
--
-- The adapter used to read opencode's `session` table, where each row is a
-- running total for a whole conversation. Taken as an event, that stamped
-- every token a session ever spent at the moment it was last touched, gave
-- all of them the model and reasoning effort the session happened to end on,
-- and reported a week of work as a single response. Here it made 1.08 billion
-- tokens arrive as 67 events.
--
-- Collector 7 reads the `message` table instead: same usage, one row per
-- response, each with its own time, model, provider and variant. The totals
-- reconcile exactly -- 6,002 messages against 77 session rows, both $8.5274 --
-- but the new events carry new ids, so without this the same spend is counted
-- under both.
--
-- native_id = session_id identifies them precisely. The session reader set
-- both to the session id; the message reader sets native_id to the message id,
-- and opencode namespaces the two ("ses_..." and "msg_..."), so no row this
-- deletes can be one of the replacements. Nothing else is touched, and the
-- rollup table holds no opencode rows to reconcile.
--
-- An agent still on collector 6 would re-send the rollups after this runs and
-- double the figures again until it upgrades. Both halves ship from one tag,
-- so that window is an upgrade in progress rather than a steady state.
DELETE FROM event WHERE source = 'opencode' AND native_id = session_id;

-- +goose Down

-- Irreversible by design: the rows described usage that is now recorded more
-- precisely, and restoring them would reintroduce the double count. Re-running
-- the collector against opencode's own database rebuilds the replacements.
SELECT 1;
