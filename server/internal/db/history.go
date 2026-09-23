package db

import (
	"context"
	"database/sql"
)

// Queries over all of stored history. Agents and DayRange read event_daily,
// small enough at day grain to read whole on every poll. EarliestRawDay and
// RollupsBefore ask where the rollups end, so they read event and daily_rollup
// themselves: a branch added to the view is added to them.

// AgentRow is one machine running the collector.
//
// LastSync is a heartbeat: the agent pushes every interval whether or not it
// has anything to send, so it stops only when the collector does -- the one
// question this table answers.
//
// There is deliberately no "last active" timestamp: when a named person last
// worked is surveillance, not collector health. A test asserts its absence.
type AgentRow struct {
	MachineID    string `json:"machine_id"`
	Hostname     string `json:"hostname"`
	Person       string `json:"person"`
	AgentVersion string `json:"agent_version"`
	FirstSeen    int64  `json:"first_seen"`
	LastSync     int64  `json:"last_sync"`
	Events       int64  `json:"events"`
}

// agentsQuery runs on every poll over all of history, so it counts from
// event_daily -- both branches, so a pruned day still counts -- and never
// groups event's rows.
const agentsQuery = `
	WITH per AS MATERIALIZED (
	  SELECT machine_id, account_ref, SUM(events) AS n
	  FROM event_daily GROUP BY machine_id, account_ref
	)
	SELECT m.id, m.hostname, m.agent_version, m.first_seen, m.last_seen,
	       COALESCE(t.events, 0),
	       COALESCE(NULLIF(a.email, ''), COALESCE(o.account_ref, ''))
	FROM machine m
	-- Counted apart from the dominant account, whose pick must not drop
	-- the other logins' events from the machine's total.
	LEFT JOIN (
		SELECT machine_id, SUM(n) AS events FROM per GROUP BY machine_id
	) t ON t.machine_id = m.id
	-- The account behind most of the machine's events: the person to go
	-- and talk to. No MAX(ts) is selected -- see the note on AgentRow.
	LEFT JOIN (
		SELECT machine_id, account_ref,
		       ROW_NUMBER() OVER (
		         PARTITION BY machine_id ORDER BY n DESC, account_ref
		       ) AS rn
		FROM per
	) o ON o.machine_id = m.id AND o.rn = 1
	LEFT JOIN account a ON a.ref = o.account_ref
	ORDER BY m.last_seen DESC, m.id`

// Agents lists every known machine, most recently synced first. It takes no
// date window: a machine that went silent three weeks ago must not vanish from
// a 7-day view, exactly when it most needs looking at.
func (d *DB) Agents(ctx context.Context) ([]AgentRow, error) {
	rows, err := d.read.QueryContext(ctx, agentsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AgentRow, 0)
	for rows.Next() {
		var r AgentRow
		if err := rows.Scan(&r.MachineID, &r.Hostname, &r.AgentVersion,
			&r.FirstSeen, &r.LastSync, &r.Events, &r.Person); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DayRange reports the extent of stored history, so the dashboard can say how
// far back the data actually goes rather than implying it is complete.
func (d *DB) DayRange(ctx context.Context) (first, last string, err error) {
	var f, l sql.NullString
	err = d.read.QueryRowContext(ctx, dayRangeQuery).Scan(&f, &l)
	return f.String, l.String, err
}

// dayRangeQuery is DayRange's statement.
const dayRangeQuery = `SELECT MIN(day), MAX(day) FROM event_daily`

// EarliestRawDay is the first day from which every day is answerable from
// individual events, or "" when there are none.
//
// After the last rolled-up day, not merely the first raw one: a late event
// stored beside a day's rollup would otherwise pull the heatmap into days it
// holds a sliver of, and it would read low beside the totals.
func (d *DB) EarliestRawDay(ctx context.Context) (string, error) {
	var v sql.NullString
	if err := d.read.QueryRowContext(ctx, `
		SELECT MAX(first, COALESCE(after, first)) FROM (
		  SELECT (SELECT MIN(day) FROM event) AS first,
		         (SELECT date(MAX(day), '+1 day') FROM daily_rollup) AS after)`,
	).Scan(&v); err != nil {
		return "", err
	}
	return v.String, nil
}

// RollupsBefore reports the day after the last rolled-up day, or "" when
// nothing is rolled up. A rollup's cost is frozen at whatever the table said
// the night it was pruned -- it keeps sums, not the per-event dimensions
// pricing needs -- so a reprice is split there.
func (d *DB) RollupsBefore(ctx context.Context) (string, error) {
	var day sql.NullString
	err := d.read.QueryRowContext(ctx,
		`SELECT date(MAX(day), '+1 day') FROM daily_rollup`).Scan(&day)
	return day.String, err
}
