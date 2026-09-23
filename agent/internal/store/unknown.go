package store

import (
	"context"
	"time"
)

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

// ClearUnknown drops the detected set so the next scan rebuilds it from what is
// on disk: a harness that gains an adapter or is uninstalled drops out, and
// anything still present is re-detected immediately.
func (s *Store) ClearUnknown(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM unknown_source`)
	return err
}
