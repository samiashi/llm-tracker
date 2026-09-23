package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

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
	defer tx.Rollback()
	n, err := deleteScope(ctx, tx, sc)
	if err != nil {
		return n, err
	}
	return n, tx.Commit()
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
