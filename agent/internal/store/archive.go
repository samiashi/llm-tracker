package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Table is one of the archive's two upload queues. Its name is formatted into
// SQL, so callers pass these constants rather than a string of their own.
type Table string

const (
	Events Table = "event"
	Quota  Table = "quota"
)

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
func putAllTx(ctx context.Context, tx *sql.Tx, table Table, items []Record) (int, error) {
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
func (s *Store) Unsent(ctx context.Context, table Table, limit int) (ids []string, payloads []json.RawMessage, err error) {
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
func (s *Store) EachPayload(ctx context.Context, table Table, fn func(json.RawMessage) error) error {
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
func (s *Store) Refused(ctx context.Context, table Table, before time.Time) (int64, error) {
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
func (s *Store) Requeue(ctx context.Context, table Table, from time.Time) (int64, error) {
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
func (s *Store) MarkSent(ctx context.Context, table Table, ids []string) error {
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

// Stats reports local counts, used by `agent status`.
func (s *Store) Stats(ctx context.Context) (events, unsent int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(sent = 0), 0) FROM event`).Scan(&events, &unsent)
	return
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
