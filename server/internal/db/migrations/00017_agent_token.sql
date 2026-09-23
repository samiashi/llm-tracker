-- +goose Up

-- Ingest tokens issued by enrolment, one row per enrolment. Only a hash is
-- kept, so a copy of the database -- a backup, a disk snapshot -- holds no
-- credential. Revoked rows stay, as the record of who had access and when.
-- NOCASE because GitHub logins are case-insensitive: revoking "Alice" must
-- reach the tokens issued to "alice".
CREATE TABLE agent_token (
    hash       TEXT PRIMARY KEY,
    login      TEXT NOT NULL COLLATE NOCASE,
    hostname   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
) WITHOUT ROWID;

CREATE INDEX idx_agent_token_login ON agent_token(login);

-- +goose Down
DROP TABLE IF EXISTS agent_token;
