-- +goose Up

-- Sessions were looked up by a correlated subquery with no index to land on,
-- so ranking the top sessions scanned the whole event table once per session.
-- At a million rows and 167 sessions that was a 33-second query, which is
-- what made the dashboard appear not to load at all.
CREATE INDEX idx_event_session ON event(session_id, ts);

-- +goose Down
DROP INDEX idx_event_session;
