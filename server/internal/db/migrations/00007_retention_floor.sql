-- +goose Up

-- Server-side settings. Currently just the retention floor.
CREATE TABLE setting (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- inference_geo was applied as a 1.1x multiplier at ingest but never stored,
-- so a reprice could not reconstruct it and silently dropped 10% from every
-- US-pinned event. Same class of problem as speed, which was stored.
ALTER TABLE event ADD COLUMN inference_geo TEXT NOT NULL DEFAULT '';
ALTER TABLE daily_rollup ADD COLUMN inference_geo TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE daily_rollup DROP COLUMN inference_geo;
ALTER TABLE event DROP COLUMN inference_geo;
DROP TABLE setting;
