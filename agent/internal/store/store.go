// Package store is the agent's durable local state: cursors, so a pass reads
// only the bytes appended since the last one, and the event archive. Claude
// Code hard-deletes transcripts older than 30 days, so the source directories
// are a feed, never a backup.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const ddl = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;

-- One row per source file, tracking how far into it we have read.
CREATE TABLE IF NOT EXISTS cursor (
  path       TEXT PRIMARY KEY,
  offset     INTEGER NOT NULL DEFAULT 0,
  size       INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

-- The durable archive. Primary key is the schema idempotency key, so
-- re-reading a file can never create a duplicate row.
CREATE TABLE IF NOT EXISTS event (
  id           TEXT PRIMARY KEY,
  ts           INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  collector    INTEGER NOT NULL DEFAULT 0,
  payload      TEXT NOT NULL,
  sent         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_event_unsent ON event(sent, ts);

CREATE TABLE IF NOT EXISTS quota (
  id           TEXT PRIMARY KEY,
  ts           INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  collector    INTEGER NOT NULL DEFAULT 0,
  payload      TEXT NOT NULL,
  sent         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_quota_unsent ON quota(sent, ts);

-- Harness directories found on this machine that we have no adapter for.
CREATE TABLE IF NOT EXISTS unknown_source (
  path       TEXT PRIMARY KEY,
  hint       TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  first_seen INTEGER NOT NULL,
  sent       INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'todo',
  note       TEXT NOT NULL DEFAULT ''
);

-- When each account became the active one.
--
-- Claude Code transcripts carry no account field whatsoever, so the only
-- signal is which account was signed in at the time. That is a single value
-- in ~/.claude.json which switching overwrites, leaving no history — so the
-- history is kept here instead, and an event is attributed by its own
-- timestamp rather than by whoever happens to be signed in when it is read.
CREATE TABLE IF NOT EXISTS account_window (
  provider   TEXT NOT NULL,
  ref        TEXT NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (provider, observed_at)
);
-- The primary key already indexes (provider, observed_at); a copy of it only
-- doubles every insert.
DROP INDEX IF EXISTS idx_account_window;

CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// Path is the archive's file in a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, "agent.db") }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite takes one writer, and the agent is single-writer by design.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// CREATE TABLE IF NOT EXISTS leaves an older store without the columns
	// added since, so they are added here. Each ALTER fails harmlessly once
	// its column exists, which is why the errors are ignored.
	for _, stmt := range []string{
		`ALTER TABLE unknown_source ADD COLUMN status TEXT NOT NULL DEFAULT 'todo'`,
		`ALTER TABLE unknown_source ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE event ADD COLUMN collector INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE quota ADD COLUMN collector INTEGER NOT NULL DEFAULT 0`,
	} {
		_, _ = db.ExecContext(context.Background(), stmt)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Cursor returns how far we have read into path, and the size we last saw.
func (s *Store) Cursor(ctx context.Context, path string) (offset, size int64, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT offset, size FROM cursor WHERE path = ?`, path)
	err = row.Scan(&offset, &size)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return offset, size, err
}

func (s *Store) Meta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, k, v string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// CommitFile stores a file's events, quota, parser state and read position in
// one transaction, and is the only way rows reach the archive (invariant 2).
//
// A cursor that commits before its rows loses them for good when the pass is
// interrupted: nothing reads those bytes again, and Claude Code deletes its
// transcripts after 30 days. meta is parser state the resumed read depends on,
// such as Codex's per-rollout header, and must move with the cursor, or the
// next pass resumes mid-file without it and stores the same responses again.
// An empty path stores rows with no cursor, for sources that are not files.
func (s *Store) CommitFile(
	ctx context.Context,
	path string, offset, size int64,
	events, quota []Record,
	meta map[string]string,
) (stored int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	n, err := putAllTx(ctx, tx, Events, events)
	if err != nil {
		return 0, err
	}
	if _, err := putAllTx(ctx, tx, Quota, quota); err != nil {
		return n, err
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta (k, v) VALUES (?, ?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v); err != nil {
			return n, err
		}
	}
	if path != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cursor (path, offset, size, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET offset=excluded.offset, size=excluded.size,
			  updated_at=excluded.updated_at`,
			path, offset, size, time.Now().Unix()); err != nil {
			return n, err
		}
	}
	return n, tx.Commit()
}
