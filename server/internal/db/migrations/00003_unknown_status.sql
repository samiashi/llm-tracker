-- +goose Up

-- Why a detected harness has no adapter.
--
-- Without this the list is a backlog that quietly regrows: a harness gets
-- requested, investigated, found unsupportable, and months later the same row
-- prompts the same investigation. A blocked row carries the finding with it.
ALTER TABLE unknown_source ADD COLUMN status TEXT NOT NULL DEFAULT 'todo';
ALTER TABLE unknown_source ADD COLUMN note   TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE unknown_source DROP COLUMN note;
ALTER TABLE unknown_source DROP COLUMN status;
