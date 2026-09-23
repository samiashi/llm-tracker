package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AccountWindow is one span during which an account was the active one. An
// empty Ref is a span in which nobody was signed in.
type AccountWindow struct {
	Provider   string
	Ref        string
	ObservedAt time.Time
}

// RecordActiveAccount appends a window when the active account has changed.
// Called once per pass, so a switch is located to within one interval.
//
// An empty ref records a sign-out, or a move to an API key: unrecorded, the
// account signed in before keeps being credited with usage it did not run.
// Only a signed-in window has anything to end, so a provider nobody ever
// signed in to records nothing.
func (s *Store) RecordActiveAccount(ctx context.Context, provider, ref string) error {
	if provider == "" {
		return nil
	}
	var last string
	err := s.db.QueryRowContext(ctx,
		`SELECT ref FROM account_window WHERE provider = ?
		 ORDER BY observed_at DESC LIMIT 1`, provider).Scan(&last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if ref == "" {
			return nil
		}
	case err != nil:
		return err
	case last == ref:
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
