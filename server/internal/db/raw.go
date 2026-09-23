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

// sourceHealthQuery runs on every poll over all of history, so it counts from
// event_day and finds each last event by one seek down
// idx_event_source_machine_ts, never grouping event's rows.
const sourceHealthQuery = `
	SELECT g.source, g.machine_id,
	       COALESCE((SELECT MAX(ts) FROM event
	                 WHERE source = g.source AND machine_id = g.machine_id), 0) AS last_event,
	       g.events
	FROM (SELECT source, machine_id, SUM(events) AS events
	      FROM event_day GROUP BY source, machine_id) g
	ORDER BY last_event DESC, g.source, g.machine_id`

// SourceHealth reads live events only, never rollups, because it needs
// per-event timestamps: it sees the retention window, which is what it is for.
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

	// Grouped on the local hour as an integer, and only each bucket turned
	// into a date: formatting every row's timestamp doubled the query.
	rows, err := d.read.QueryContext(ctx, `
		SELECT (ts + ?) / 3600 AS hour, SUM(total_tokens), COUNT(*)
		FROM event WHERE `+where+`
		GROUP BY hour ORDER BY hour`,
		append([]any{offset}, args...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]HeatCell, 0)
	for rows.Next() {
		var hour int64
		var c HeatCell
		if err := rows.Scan(&hour, &c.Tokens, &c.Events); err != nil {
			return nil, 0, err
		}
		local := time.Unix(hour*3600, 0).UTC()
		c.Day, c.Weekday, c.Hour = local.Format("2006-01-02"), int(local.Weekday()), local.Hour()
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
	UnpricedTokens  int64   `json:"unpriced_tokens"`
	Events          int64   `json:"events"`
	LastSeen        int64   `json:"last_seen"`
}

// sessionsList is how many sessions TopSessions returns.
var sessionsList = listLen{def: 10, max: 100}

// TopSessions ranks sessions by cost.
func (d *DB) TopSessions(ctx context.Context, w Window, limit int) ([]SessionRow, error) {
	w = w.Normalise()
	where, args := w.where("")

	// One pass over the range, grouped as finely as each session's dominant
	// model and effort need: whichever accounted for most of its tokens
	// describes its cost better than whichever was set last.
	q := `
		-- MATERIALIZED because scoped is read three times below, and inlined
		-- each read walks the window again.
		WITH scoped AS MATERIALIZED (
		  SELECT session_id, source, model, effort,
		         SUM(total_tokens) AS tokens,
		         ` + billedUSD + ` AS billed,
		         ` + rateCardUSD + ` AS ratecard,
		         ` + unknownBasisUSD + ` AS unknown_basis,
		         ` + unpricedUsage + ` AS unpriced,
		         COUNT(*) AS events, MAX(ts) AS last_seen,
		         MAX(account_ref) AS account_ref
		  FROM event WHERE ` + where + ` AND session_id != ''
		  GROUP BY session_id, source, model, effort
		),
		agg AS (
		  SELECT session_id, source,
		         SUM(tokens) AS tokens, SUM(billed) AS billed, SUM(ratecard) AS ratecard,
		         SUM(unknown_basis) AS unknown_basis, SUM(unpriced) AS unpriced,
		         SUM(events) AS events, MAX(last_seen) AS last_seen,
		         MAX(account_ref) AS account_ref
		  FROM scoped GROUP BY session_id, source
		),
		-- By whichever figure describes the session, never by their sum: seat
		-- usage has no marginal cost. And not one column then the next, which
		-- ranks every metered session above every seat one whatever the cost.
		--
		-- A session with unpriced tokens has no cost to rank by, and ranked
		-- by its $0 it would never show (invariant 8). It is placed by its
		-- unpriced tokens against every session's tokens, if that is higher.
		ranked AS (
		  SELECT *,
		         ROW_NUMBER() OVER (ORDER BY MAX(billed, ratecard, unknown_basis) DESC,
		                                     tokens DESC, session_id, source) AS by_cost,
		         ROW_NUMBER() OVER (ORDER BY CASE WHEN unpriced > 0 THEN unpriced ELSE tokens END DESC,
		                                     session_id, source) AS by_volume
		  FROM agg
		),
		-- A tie goes to the one used last, then by name or scale, so a
		-- session's model and effort cannot swap between two polls.
		top_model AS (
		  SELECT session_id, model,
		         ROW_NUMBER() OVER (PARTITION BY session_id
		           ORDER BY SUM(tokens) DESC, MAX(last_seen) DESC, model) AS rn
		  FROM scoped WHERE model != '' GROUP BY session_id, model
		),
		top_effort AS (
		  SELECT session_id, effort,
		         ROW_NUMBER() OVER (PARTITION BY session_id
		           ORDER BY SUM(tokens) DESC, MAX(last_seen) DESC, ` + effortOrder("effort") + `) AS rn
		  FROM scoped WHERE effort != '' GROUP BY session_id, effort
		)
		SELECT a.session_id, a.source,
		       COALESCE(m.model,''), COALESCE(f.effort,''), COALESCE(acc.email,''),
		       a.tokens, a.billed, a.ratecard, a.unknown_basis, a.unpriced,
		       a.events, a.last_seen
		FROM ranked a
		LEFT JOIN top_model  m ON m.session_id = a.session_id AND m.rn = 1
		LEFT JOIN top_effort f ON f.session_id = a.session_id AND f.rn = 1
		LEFT JOIN account  acc ON acc.ref = a.account_ref
		ORDER BY CASE WHEN a.unpriced > 0 THEN MIN(a.by_cost, a.by_volume) ELSE a.by_cost END,
		         a.by_cost
		LIMIT ?`

	rows, err := d.read.QueryContext(ctx, q, append(args, sessionsList.of(limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionRow, 0)
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.SessionID, &s.Source, &s.Model, &s.Effort, &s.Email,
			&s.Tokens, &s.BilledUSD, &s.RateCardUSD, &s.UnknownBasisUSD, &s.UnpricedTokens,
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
