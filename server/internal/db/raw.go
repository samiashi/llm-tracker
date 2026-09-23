package db

import (
	"context"
	"time"
)

// Queries event_daily cannot answer, exempt from invariant 5: they need an
// hour, a session or an event's own time, which a day-level rollup does not
// hold, so they see only days not yet rolled up.

// SourceHealth reports when each source last produced an event, per machine.
// It is the canary: these formats are private and change without notice, and
// the realistic failure is a source that quietly stops reporting.
type SourceHealth struct {
	Source    string `json:"source"`
	MachineID string `json:"machine_id"`
	LastEvent int64  `json:"last_event"`
	Events    int64  `json:"events"`
}

// sourceHealthQuery runs on every poll over all of history, so
// idx_event_source_machine_ts must serve the grouping in order rather than a
// sort of every row.
const sourceHealthQuery = `
	SELECT source, machine_id, MAX(ts), COUNT(*)
	FROM event GROUP BY source, machine_id ORDER BY MAX(ts) DESC`

// SourceHealth reads raw events because it needs per-event timestamps, so it
// only sees the retention window -- which is what it is for.
func (d *DB) SourceHealth(ctx context.Context) ([]SourceHealth, error) {
	rows, err := d.read.QueryContext(ctx, sourceHealthQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SourceHealth, 0)
	for rows.Next() {
		var s SourceHealth
		if err := rows.Scan(&s.Source, &s.MachineID, &s.LastEvent, &s.Events); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// HeatCell is one date/hour bucket of activity, keyed by date rather than
// weekday to answer "what ran at 03:00 on the 21st". Weekday comes along so the
// client labels the row without re-deriving it in another time zone.
type HeatCell struct {
	Day     string `json:"day"`
	Weekday int    `json:"weekday"` // 0 = Sunday
	Hour    int    `json:"hour"`
	Tokens  int64  `json:"tokens"`
	Events  int64  `json:"events"`
}

// Heatmap buckets activity by local date and hour, and returns the offset it
// used, in minutes east of UTC, so the dashboard can name the zone.
//
// The local offset is computed once in Go and applied as integer arithmetic:
// strftime(...,'localtime') on every row is opaque to the planner and far
// slower. The cost is that a range spanning a DST change uses one offset
// throughout, so hours past the change shift by one. Exact buckets would need
// each agent's own offset per event; the server's zone is not the developer's
// anyway.
func (d *DB) Heatmap(ctx context.Context, w Window) ([]HeatCell, int, error) {
	w = w.Normalise()

	// Clamped to the days raw events answer in full: an hourly bucket cannot
	// come from a day-level rollup, and unclamped the card would report a
	// fraction of the totals beside it.
	if first, err := d.EarliestRawDay(ctx); err != nil {
		return nil, 0, err
	} else if first != "" && w.From < first {
		w.From = first
	}

	// Offset at the midpoint of the range, so a range mostly inside DST uses
	// the offset that covers most of it.
	from, _ := time.ParseInLocation("2006-01-02", w.From, time.Local)
	to, _ := time.ParseInLocation("2006-01-02", w.To, time.Local)
	mid := from.Add(to.Sub(from) / 2)
	_, offset := mid.Zone()
	if w.From > w.To {
		return make([]HeatCell, 0), offset / 60, nil
	}

	// The indexed UTC day selects the events, as it does for Totals, so the
	// two agree by construction; local time only picks the bucket. A bucket
	// may therefore sit on a local day just outside the range: it is the same
	// event the total counts.
	where, args := w.where("")

	rows, err := d.read.QueryContext(ctx, `
		SELECT local_day,
		       CAST(strftime('%w', local_day) AS INTEGER),
		       local_hour,
		       SUM(total_tokens), SUM(events)
		FROM (
		  SELECT date((ts + ?), 'unixepoch')                        AS local_day,
		         CAST(strftime('%H', (ts + ?), 'unixepoch') AS INTEGER) AS local_hour,
		         total_tokens, 1 AS events
		  FROM event WHERE `+where+`
		)
		GROUP BY local_day, local_hour
		ORDER BY local_day, local_hour`,
		append([]any{offset, offset}, args...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]HeatCell, 0)
	for rows.Next() {
		var c HeatCell
		if err := rows.Scan(&c.Day, &c.Weekday, &c.Hour, &c.Tokens, &c.Events); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, offset / 60, rows.Err()
}

// SessionRow is one session's totals.
//
// It carries no project path: the table does not show one, and every viewer
// of the dashboard would receive every colleague's working directories.
type SessionRow struct {
	SessionID       string  `json:"session_id"`
	Source          string  `json:"source"`
	Model           string  `json:"model"`
	Effort          string  `json:"effort"`
	Email           string  `json:"email"`
	Tokens          int64   `json:"tokens"`
	BilledUSD       float64 `json:"billed_usd"`
	RateCardUSD     float64 `json:"rate_card_usd"`
	UnknownBasisUSD float64 `json:"unknown_basis_usd"`
	Events          int64   `json:"events"`
	LastSeen        int64   `json:"last_seen"`
}

// TopSessions ranks sessions by cost.
func (d *DB) TopSessions(ctx context.Context, w Window, limit int) ([]SessionRow, error) {
	w = w.Normalise()
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	where, args := w.where("")

	// One pass over the range, then window functions to pick each session's
	// dominant model and effort: whichever accounted for most of its tokens
	// describes its cost better than whichever was set last.
	q := `
		-- MATERIALIZED because scoped is read three times below, and inlined
		-- each read walks the window again. Only the columns used, so the copy
		-- stays small over a long range.
		WITH scoped AS MATERIALIZED (
		  SELECT session_id, source, ts, account_ref, model, effort,
		         total_tokens, cost_basis, cost_usd
		  FROM event WHERE ` + where + ` AND session_id != ''
		),
		agg AS (
		  SELECT session_id, source,
		         SUM(total_tokens) AS tokens,
		         ` + billedUSD + ` AS billed,
		         ` + rateCardUSD + ` AS ratecard,
		         ` + unknownBasisUSD + ` AS unknown_basis,
		         COUNT(*) AS events, MAX(ts) AS last_seen,
		         MAX(account_ref) AS account_ref
		  FROM scoped GROUP BY session_id, source
		),
		top_model AS (
		  SELECT session_id, model,
		         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY SUM(total_tokens) DESC) AS rn
		  FROM scoped WHERE model != '' GROUP BY session_id, model
		),
		top_effort AS (
		  SELECT session_id, effort,
		         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY SUM(total_tokens) DESC) AS rn
		  FROM scoped WHERE effort != '' GROUP BY session_id, effort
		)
		SELECT a.session_id, a.source,
		       COALESCE(m.model,''), COALESCE(f.effort,''), COALESCE(acc.email,''),
		       a.tokens, a.billed, a.ratecard, a.unknown_basis, a.events, a.last_seen
		FROM agg a
		LEFT JOIN top_model  m ON m.session_id = a.session_id AND m.rn = 1
		LEFT JOIN top_effort f ON f.session_id = a.session_id AND f.rn = 1
		LEFT JOIN account  acc ON acc.ref = a.account_ref
		-- By whichever figure describes the session, never by their sum: seat
		-- usage has no marginal cost. And not one column then the next, which
		-- ranks every metered session above every seat one whatever the cost.
		ORDER BY MAX(a.billed, a.ratecard, a.unknown_basis) DESC, a.tokens DESC
		LIMIT ?`

	rows, err := d.read.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionRow, 0)
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.SessionID, &s.Source, &s.Model, &s.Effort, &s.Email,
			&s.Tokens, &s.BilledUSD, &s.RateCardUSD, &s.UnknownBasisUSD,
			&s.Events, &s.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UnknownRow is a harness detected on a machine with no adapter for it.
type UnknownRow struct {
	MachineID string `json:"machine_id"`
	Path      string `json:"path"`
	Hint      string `json:"hint"`
	SizeBytes int64  `json:"size_bytes"`
	LastSeen  int64  `json:"last_seen"`
	Status    string `json:"status"`
	Note      string `json:"note"`
}

func (d *DB) UnknownSources(ctx context.Context) ([]UnknownRow, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT machine_id, path, hint, size_bytes, last_seen, status, note
		 FROM unknown_source
		 -- Actionable rows first; blocked ones are a record, not a queue.
		 ORDER BY status = 'blocked', size_bytes DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UnknownRow, 0)
	for rows.Next() {
		var u UnknownRow
		if err := rows.Scan(&u.MachineID, &u.Path, &u.Hint, &u.SizeBytes, &u.LastSeen,
			&u.Status, &u.Note); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
