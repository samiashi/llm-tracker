-- +goose Up

-- The GitHub login each machine and account belongs to. A batch names its
-- machine and accounts itself, and every enrolled token can upload, so with no
-- owner recorded any colleague's token could rename another's machine, move
-- their email onto a ref of its own, or fire a re-key trigger that deletes
-- their rows. A machine is claimed by the first login to upload from it, an
-- account by the first to report it; NULL is unclaimed, and rows stored before
-- this are claimed by the next upload that names them. NOCASE as in
-- agent_token: GitHub logins are case-insensitive.
ALTER TABLE machine ADD COLUMN login TEXT COLLATE NOCASE;
ALTER TABLE account ADD COLUMN login TEXT COLLATE NOCASE;

-- +goose Down
ALTER TABLE account DROP COLUMN login;
ALTER TABLE machine DROP COLUMN login;
