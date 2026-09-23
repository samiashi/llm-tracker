-- +goose Up

-- project_path and git_branch were uploaded with every event and never read:
-- no query, export, view or trigger touches them. A path names the
-- developer's macOS account and the project or client they work on, which is
-- not the server's to keep for nothing. No index, view or trigger references
-- either column, which is what lets SQLite drop them.
ALTER TABLE event DROP COLUMN project_path;
ALTER TABLE event DROP COLUMN git_branch;

-- +goose Down
-- The values are gone; the columns come back empty.
ALTER TABLE event ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
ALTER TABLE event ADD COLUMN git_branch   TEXT NOT NULL DEFAULT '';
