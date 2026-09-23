// Package store is the agent's durable local state: cursors, so a pass reads
// only the bytes appended since the last one, and the event archive. Claude
// Code hard-deletes transcripts older than 30 days, so the source directories
// are a feed, never a backup.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Record is one row ready for storage.
//
// TotalTokens exists to resolve conflicts: Claude Code writes several lines
// for one streamed response, each with a larger running total, and
// insert-or-ignore would keep the first and smallest.
type Record struct {
	ID          string
	TS          time.Time
	TotalTokens int64
	// Collector is the version of the code that produced this row's payload.
	// A newer one replaces an equal reading, which is how a corrected adapter
	// upgrades rows it wrote before.
	Collector int
	Payload   any
}

// putAllTx writes rows under the archive's one conflict rule (invariant 1).
//
// A row moves as a unit, and only to a better reading of its id: more tokens,
// or the same tokens read by a newer collector. One id meets smaller, equal and
// larger readings in any order -- streamed lines, and responses repeated across
// resumed and forked sessions. A column moved on its own leaves the row
// claiming what its payload, the half Unsent ships, does not hold: a count it
// does not carry, or a collector whose own reading then ties and never lands.
// Each SET repeats the guard so a WHERE arm added later still cannot lower
// anything.
func putAllTx(ctx context.Context, tx *sql.Tx, table string, items []Record) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}

	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s (id, ts, total_tokens, collector, payload, sent)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET
		  ts           = CASE WHEN excluded.total_tokens >= %[1]s.total_tokens
		                      THEN excluded.ts ELSE %[1]s.ts END,
		  total_tokens = MAX(excluded.total_tokens, %[1]s.total_tokens),
		  collector    = CASE WHEN excluded.total_tokens >= %[1]s.total_tokens
		                      THEN excluded.collector ELSE %[1]s.collector END,
		  payload      = CASE WHEN excluded.total_tokens >= %[1]s.total_tokens
		                      THEN excluded.payload ELSE %[1]s.payload END,
		  sent         = 0
		WHERE excluded.total_tokens > %[1]s.total_tokens
		   OR (excluded.total_tokens = %[1]s.total_tokens
		       AND excluded.collector > %[1]s.collector)`, table))
	if err != nil {
		return 0, err
	}
	defer stmt.Close() //nolint:errcheck

	n := 0
	for _, it := range items {
		b, err := json.Marshal(it.Payload)
		if err != nil {
			return n, err
		}
		res, err := stmt.ExecContext(ctx, it.ID, it.TS.Unix(), it.TotalTokens, it.Collector, string(b))
		if err != nil {
			return n, err
		}
		if ra, _ := res.RowsAffected(); ra > 0 {
			n++
		}
	}
	return n, nil
}

// Unsent returns up to limit rows awaiting upload, oldest first.
func (s *Store) Unsent(ctx context.Context, table string, limit int) (ids []string, payloads []json.RawMessage, err error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, payload FROM %s WHERE sent = 0 ORDER BY ts LIMIT ?`, table), limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		payloads = append(payloads, json.RawMessage(p))
	}
	return ids, payloads, rows.Err()
}

// EachPayload calls fn with every row of table, oldest first, without holding
// the table in memory.
func (s *Store) EachPayload(ctx context.Context, table string, fn func(json.RawMessage) error) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT payload FROM %s ORDER BY ts`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		if err := fn(json.RawMessage(p)); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Refused marks rows the server will not accept because they predate its
// retention floor, and returns how many.
//
// A third state, sent = 2. Marked sent, they would claim a delivery that did
// not happen, and a resend against a pruned server would lose them for good.
// Left unsent, they sit at the head of the oldest-first queue, every push fails
// on them, and nothing newer ever leaves the machine.
func (s *Store) Refused(ctx context.Context, table string, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET sent = 2 WHERE sent = 0 AND ts < ?`, table),
		before.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountRefused reports how many events the server has refused, which `status`
// shows apart from the pending count: they are neither delivered nor pending.
func (s *Store) CountRefused(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event WHERE sent = 2`).Scan(&n)
	return n, err
}

// Requeue offers refused rows again, from the day `from` on (every refused row
// when from is zero), and returns how many. Refusal is not permanent: a server
// whose floor drops can take those rows back.
func (s *Store) Requeue(ctx context.Context, table string, from time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET sent = 0 WHERE sent = 2 AND ts >= ?`, table), from.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MarkSent flags rows as uploaded. Rows are kept, not deleted: this table is
// the archive, and the upstream server is not a substitute for it.
func (s *Store) MarkSent(ctx context.Context, table string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`UPDATE %s SET sent = 1 WHERE id = ?`, table))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PutUnknownSource(ctx context.Context, path, hint string, size int64, status, note string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO unknown_source (path, hint, size_bytes, first_seen, sent, status, note)
		VALUES (?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  size_bytes = excluded.size_bytes,
		  hint       = excluded.hint,
		  status     = excluded.status,
		  note       = excluded.note`,
		path, hint, size, time.Now().Unix(), status, note)
	return err
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

// Stats reports local counts, used by `agent status`.
func (s *Store) Stats(ctx context.Context) (events, unsent int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(sent = 0), 0) FROM event`).Scan(&events, &unsent)
	return
}

// UnknownRow is a detected-but-unsupported harness directory.
type UnknownRow struct {
	Path      string
	Hint      string
	SizeBytes int64
	FirstSeen time.Time
	Status    string
	Note      string
}

// AllUnknown returns every unsupported harness currently detected.
func (s *Store) AllUnknown(ctx context.Context) ([]UnknownRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, hint, size_bytes, first_seen, status, note FROM unknown_source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnknownRow
	for rows.Next() {
		var u UnknownRow
		var ts int64
		if err := rows.Scan(&u.Path, &u.Hint, &u.SizeBytes, &ts, &u.Status, &u.Note); err != nil {
			return nil, err
		}
		u.FirstSeen = time.Unix(ts, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// MarkAllUnsent re-queues the whole archive for upload, for a rebuilt server
// or one with a new column the stored rows should fill. The server's insert is
// idempotent, so re-pushing costs only bandwidth.
func (s *Store) MarkAllUnsent(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE event SET sent = 0`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `UPDATE quota SET sent = 0`); err != nil {
		return n, err
	}
	return n, nil
}

// Scope is everything one source owns in this store: the cursors under its
// directories and the parser state under its meta keys.
//
// The adapter decides it (sources.ScopeOf) and this package only clears it: a
// copy of the adapters' roots kept here, where they cannot see it, drifts from
// them and rewinds another source's cursors, or none.
type Scope struct {
	// CursorPrefixes are absolute directory paths, each ending in a separator.
	// A cursor belongs to the source when its path starts with one of them.
	CursorPrefixes []string
	// MetaPrefixes are the key prefixes of the source's watermarks and
	// per-file parser state.
	MetaPrefixes []string
}

// deleteScope clears a scope's cursors and meta inside tx, returning how many
// cursors went.
//
// Matched with instr(...) = 1, a literal prefix test. LIKE would read the `_`
// in a name such as roo_code as a wildcard.
func deleteScope(ctx context.Context, tx *sql.Tx, sc Scope) (int64, error) {
	var n int64
	for _, p := range sc.CursorPrefixes {
		res, err := tx.ExecContext(ctx, `DELETE FROM cursor WHERE instr(path, ?) = 1`, p)
		if err != nil {
			return n, err
		}
		m, _ := res.RowsAffected()
		n += m
	}
	for _, p := range sc.MetaPrefixes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE instr(k, ?) = 1`, p); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ResetCursors rewinds a source's read positions without touching the archive:
// the next pass re-reads it from the beginning and the conflict rule upgrades
// rows in place.
func (s *Store) ResetCursors(ctx context.Context, sc Scope) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	n, err := deleteScope(ctx, tx, sc)
	if err != nil {
		return n, err
	}
	return n, tx.Commit()
}

// ClearUnknown drops the detected set so the next scan rebuilds it from what is
// on disk: a harness that gains an adapter or is uninstalled drops out, and
// anything still present is re-detected immediately.
func (s *Store) ClearUnknown(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM unknown_source`)
	return err
}

// CountSource reports how many stored events belong to a source, so a
// destructive command can say what it is about to destroy before it does.
func (s *Store) CountSource(ctx context.Context, source string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event WHERE json_extract(payload, '$.source') = ?`,
		source).Scan(&n)
	return n, err
}

// SourcePayloads returns the payload of every stored event from one source.
func (s *Store) SourcePayloads(ctx context.Context, source string) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM event WHERE json_extract(payload, '$.source') = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(p))
	}
	return out, rows.Err()
}

// DeleteEvents removes events by id in one transaction and returns how many.
func (s *Store) DeleteEvents(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `DELETE FROM event WHERE id = ?`, id)
		if err != nil {
			return 0, err
		}
		m, _ := res.RowsAffected()
		n += m
	}
	return n, tx.Commit()
}

// ResetSource deletes a source's rows along with its scope, so the next pass
// rebuilds them from scratch. For rows no re-read will reach again, such as
// events whose ids an adapter changed.
//
// The rows go before they are rebuilt, so anything whose source file has since
// been deleted is lost rather than re-read: Claude Code and Cowork discard
// their transcripts after 30 days.
func (s *Store) ResetSource(ctx context.Context, source string, sc Scope) (deleted int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM event WHERE json_extract(payload, '$.source') = ?`, source)
	if err != nil {
		return 0, err
	}
	deleted, _ = res.RowsAffected()

	if _, err := deleteScope(ctx, tx, sc); err != nil {
		return deleted, err
	}
	return deleted, tx.Commit()
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

	n, err := putAllTx(ctx, tx, "event", events)
	if err != nil {
		return 0, err
	}
	if _, err := putAllTx(ctx, tx, "quota", quota); err != nil {
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

// AccountWindow is one span during which an account was the active one.
type AccountWindow struct {
	Provider   string
	Ref        string
	ObservedAt time.Time
}

// RecordActiveAccount appends a window when the active account has changed.
// Called once per pass, so a switch is located to within one interval.
func (s *Store) RecordActiveAccount(ctx context.Context, provider, ref string) error {
	if provider == "" || ref == "" {
		return nil
	}
	var last string
	err := s.db.QueryRowContext(ctx,
		`SELECT ref FROM account_window WHERE provider = ?
		 ORDER BY observed_at DESC LIMIT 1`, provider).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if last == ref {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO account_window (provider, ref, observed_at) VALUES (?, ?, ?)
		 ON CONFLICT(provider, observed_at) DO NOTHING`,
		provider, ref, time.Now().Unix())
	return err
}

// AccountWindows returns every recorded switch, oldest first.
func (s *Store) AccountWindows(ctx context.Context) ([]AccountWindow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, ref, observed_at FROM account_window ORDER BY observed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []AccountWindow
	for rows.Next() {
		var w AccountWindow
		var ts int64
		if err := rows.Scan(&w.Provider, &w.Ref, &ts); err != nil {
			return nil, err
		}
		w.ObservedAt = time.Unix(ts, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}
