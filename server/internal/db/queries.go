package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// Window is an inclusive day range plus an optional person filter.
//
// Person is an email rather than an account reference, because one colleague
// holds several logins and drilling into "them" means all of them.
type Window struct{ From, To, Person string }

// Normalise fills in the default window, the last 30 UTC days. A caller that
// reports the range it served, like the CSV filename, settles it first.
func (w Window) Normalise() Window {
	if w.To == "" {
		w.To = time.Now().UTC().Format("2006-01-02")
	}
	if w.From == "" {
		w.From = time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	}
	return w
}

// where builds the shared filter clause and its arguments.
//
// The person filter resolves through the account table as a subquery rather
// than a join, so every query can apply it the same way without each one
// growing a join it otherwise would not need.
func (w Window) where(col string) (string, []any) {
	clause := col + "day BETWEEN ? AND ?"
	args := []any{w.From, w.To}
	if w.Person != "" {
		// Match the ref directly as well as through the email: ByPerson keys
		// an account with no email by its ref, and the dashboard feeds that
		// key back as the filter, which no email matches.
		clause += " AND (" + col + "account_ref IN (SELECT ref FROM account WHERE email = ?)" +
			" OR " + col + "account_ref = ?)"
		args = append(args, w.Person, w.Person)
	}
	return clause, args
}

// Totals is the headline figure set.
//
// Billed, rate-card and unknown-basis spend are three fields, never a sum:
// one is money that left the bank, one is what seat usage would have cost on
// the API, and the third is priced usage whose adapter could not say which --
// an API-key session, Copilot. Unpriced volume is reported so it stays
// visible instead of disappearing into a zero.
type Totals struct {
	Events           int64   `json:"events"`
	TotalTokens      int64   `json:"total_tokens"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	BilledUSD        float64 `json:"billed_usd"`
	// BilledTokens is the priced usage behind BilledUSD, the only divisor
	// that gives a per-token rate for it.
	BilledTokens       int64   `json:"billed_tokens"`
	RateCardUSD        float64 `json:"rate_card_usd"`
	UnknownBasisUSD    float64 `json:"unknown_basis_usd"`
	UnknownBasisTokens int64   `json:"unknown_basis_tokens"`
	UnpricedTokens     int64   `json:"unpriced_tokens"`
	UnpricedEvents     int64   `json:"unpriced_events"`
}

// totalsSelect runs against event_daily, where a row is already an aggregate,
// so the event count is a column rather than COUNT(*).
const totalsSelect = `
  COALESCE(SUM(events),0),
  COALESCE(SUM(total_tokens),0),
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_5m + cache_write_1h),0),
  COALESCE(SUM(reasoning_tokens),0),
  ` + billedUSD + `,
  COALESCE(SUM(CASE WHEN cost_basis='billed' AND cost_source!='unpriced' THEN total_tokens ELSE 0 END),0),
  ` + rateCardUSD + `,
  ` + unknownBasisUSD + `,
  COALESCE(SUM(CASE WHEN cost_basis NOT IN ` + knownBasis + ` AND cost_source!='unpriced' THEN total_tokens ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN cost_source='unpriced' THEN total_tokens ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN cost_source='unpriced' THEN events ELSE 0 END),0)`

// knownBasis is the cost bases with a money figure of their own. Every other
// value is reported as unknown basis, so priced spend lands in exactly one of
// the three.
const knownBasis = `('billed','rate_card_equivalent')`

// The three spend columns every query reports. Separate by construction:
// invariant 3 forbids summing them.
const (
	billedUSD       = `COALESCE(SUM(CASE WHEN cost_basis='billed' THEN cost_usd ELSE 0 END),0)`
	rateCardUSD     = `COALESCE(SUM(CASE WHEN cost_basis='rate_card_equivalent' THEN cost_usd ELSE 0 END),0)`
	unknownBasisUSD = `COALESCE(SUM(CASE WHEN cost_basis NOT IN ` + knownBasis + ` THEN cost_usd ELSE 0 END),0)`
)

// totalsDest returns scan destinations in totalsSelect's column order, so the
// order is written once. The three spend fields are all float64: a swap in a
// hand-copied list would compile and scan without error.
func totalsDest(t *Totals) []any {
	return []any{&t.Events, &t.TotalTokens, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens, &t.ReasoningTokens,
		&t.BilledUSD, &t.BilledTokens, &t.RateCardUSD,
		&t.UnknownBasisUSD, &t.UnknownBasisTokens,
		&t.UnpricedTokens, &t.UnpricedEvents}
}

func scanTotals(row interface{ Scan(...any) error }) (Totals, error) {
	var t Totals
	err := row.Scan(totalsDest(&t)...)
	return t, err
}

func (d *DB) Totals(ctx context.Context, w Window) (Totals, error) {
	w = w.Normalise()
	where, args := w.where("")
	row := d.read.QueryRowContext(ctx, `SELECT `+totalsSelect+` FROM event_daily WHERE `+where, args...)
	return scanTotals(row)
}

// Group is one row of a breakdown: a key plus its totals.
type Group struct {
	Key    string `json:"key"`
	Label  string `json:"label,omitempty"`
	Totals Totals `json:"totals"`
}

// breakdown runs a grouped totals query, largest first. dimension is chosen
// from an allow-list by the caller, never interpolated from user input.
func (d *DB) breakdown(ctx context.Context, w Window, dimension, joinSQL, labelExpr string) ([]Group, error) {
	w = w.Normalise()
	label := "''"
	if labelExpr != "" {
		label = labelExpr
	}
	where, args := w.where("event.")
	// Ordered by tokens, the length every caller draws. MIN(label) because
	// the label is not grouped, and SQLite fills a bare column from whichever
	// row it read last: a model served both directly and through a gateway
	// would show a provider that changes between two loads.
	q := fmt.Sprintf(`SELECT %s, MIN(%s), %s FROM event_daily event %s WHERE %s
	                  GROUP BY %s ORDER BY SUM(event.total_tokens) DESC`,
		dimension, label, totalsSelect, joinSQL, where, dimension)
	rows, err := d.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Group, 0)
	for rows.Next() {
		var g Group
		var key, lbl sql.NullString
		if err := rows.Scan(append([]any{&key, &lbl}, totalsDest(&g.Totals)...)...); err != nil {
			return nil, err
		}
		g.Key, g.Label = key.String, lbl.String
		if g.Key == "" {
			g.Key = "unknown"
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ByPerson groups by email rather than by account: one person holds several
// accounts, and grouped by account_ref they would appear once per login with
// their usage split across the rows.
func (d *DB) ByPerson(ctx context.Context, w Window) ([]Group, error) {
	return d.breakdown(ctx, w,
		"COALESCE(NULLIF(account.email,''), event.account_ref)",
		"LEFT JOIN account ON account.ref = event.account_ref", "")
}

func (d *DB) ByModel(ctx context.Context, w Window) ([]Group, error) {
	return d.breakdown(ctx, w, "event.model", "", "event.provider")
}

func (d *DB) BySource(ctx context.Context, w Window) ([]Group, error) {
	return d.breakdown(ctx, w, "event.source", "", "event.surface")
}

func (d *DB) BySurface(ctx context.Context, w Window) ([]Group, error) {
	return d.breakdown(ctx, w, "event.surface", "", "")
}

// There is no breakdown by project or branch. Both are high-cardinality and
// absent from daily_rollup, where they would multiply its rows, so event_daily
// cannot serve one, and one read from event under-reports every pruned day.

// effortKey normalises an effort column the way EffortRank reads it, or
// "XHigh" and " high" group apart from the levels they are.
func effortKey(col string) string { return "lower(trim(" + col + "))" }

// effortOrder sorts by schema.EffortRank, then breaks a tie as
// EffortDisplayOrder does -- a level before the aliases that run at it -- then
// by value, so ultracode and xhigh never swap places between two loads.
func effortOrder(col string) string {
	display := schema.EffortDisplayOrder()
	var b strings.Builder
	b.WriteString(schema.EffortOrderSQL(col) + ", CASE " + effortKey(col))
	for i, e := range display {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", e, i)
	}
	fmt.Fprintf(&b, " ELSE %d END, %s", len(display), col)
	return b.String()
}

// Daily is one day of the time series.
type Daily struct {
	Day    string `json:"day"`
	Totals Totals `json:"totals"`
}

func (d *DB) Daily(ctx context.Context, w Window) ([]Daily, error) {
	w = w.Normalise()
	where, args := w.where("")
	rows, err := d.read.QueryContext(ctx,
		`SELECT day, `+totalsSelect+` FROM event_daily WHERE `+where+` GROUP BY day ORDER BY day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Daily, 0)
	for rows.Next() {
		var x Daily
		if err := rows.Scan(append([]any{&x.Day}, totalsDest(&x.Totals)...)...); err != nil {
			return nil, err
		}
		out = append(out, x)
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

// agentsQuery runs on every poll over all of history.
const agentsQuery = `
	-- event_daily's two branches, each grouped before the union: through
	-- the view SQLite sorts every row of history on every poll, where
	-- idx_event_machine_account reads them in order. Both branches, so a
	-- pruned day still counts.
	WITH per AS MATERIALIZED (
	  SELECT machine_id, account_ref, SUM(n) AS n FROM (
	    SELECT machine_id, account_ref, COUNT(*) AS n
	    FROM event GROUP BY machine_id, account_ref
	    UNION ALL
	    SELECT machine_id, account_ref, SUM(events)
	    FROM daily_rollup GROUP BY machine_id, account_ref
	  ) GROUP BY machine_id, account_ref
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
	ORDER BY m.last_seen DESC`

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

// dayRangeQuery reads event_daily's two branches, one index seek each: SQLite
// answers a lone MIN or MAX from an index, but a pair of them over the view
// scans all of history on every poll.
const dayRangeQuery = `
	SELECT MIN(d), MAX(d) FROM (
	  SELECT (SELECT MIN(day) FROM event) AS d
	  UNION ALL SELECT (SELECT MAX(day) FROM event)
	  UNION ALL SELECT (SELECT MIN(day) FROM daily_rollup)
	  UNION ALL SELECT (SELECT MAX(day) FROM daily_rollup))`

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

// ModelDay is one model's tokens on one day.
type ModelDay struct {
	Day    string `json:"day"`
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// DailyByModel returns a per-day series for the busiest models. The rest are
// left out, not folded into an "other" band: the chart does not stack, so it
// never claims to cover every token, and the model breakdown beside it lists
// them all.
func (d *DB) DailyByModel(ctx context.Context, w Window, top int) ([]ModelDay, error) {
	w = w.Normalise()
	if top <= 0 || top > 8 {
		top = 6
	}
	where, args := w.where("")
	q := `
		WITH ranked AS (
		  SELECT model, SUM(total_tokens) AS t FROM event_daily
		  WHERE ` + where + ` AND model != ''
		  GROUP BY model ORDER BY t DESC LIMIT ?
		)
		SELECT day, model, SUM(total_tokens)
		FROM event_daily WHERE ` + where + `
		  AND model IN (SELECT model FROM ranked)
		GROUP BY 1, 2 ORDER BY day`
	qargs := append(append([]any{}, args...), top)
	qargs = append(qargs, args...)
	rows, err := d.read.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelDay, 0)
	for rows.Next() {
		var m ModelDay
		if err := rows.Scan(&m.Day, &m.Model, &m.Tokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ByOrigin splits main-thread work from subagent fan-out.
func (d *DB) ByOrigin(ctx context.Context, w Window) ([]Group, error) {
	return d.breakdown(ctx, w,
		"CASE WHEN event.is_subagent THEN 'subagent' ELSE 'main thread' END", "", "")
}

// Compare returns totals for a window alongside the equally-sized window
// immediately before it.
type Compare struct {
	Current  Totals `json:"current"`
	Previous Totals `json:"previous"`
	// PreviousFrom and PreviousTo name the comparison window, so the
	// dashboard can say what it compares against.
	PreviousFrom string `json:"previous_from"`
	PreviousTo   string `json:"previous_to"`
}

func (d *DB) Compare(ctx context.Context, w Window) (Compare, error) {
	w = w.Normalise()
	cur, err := d.Totals(ctx, w)
	if err != nil {
		return Compare{}, err
	}

	from, err := time.Parse("2006-01-02", w.From)
	if err != nil {
		return Compare{}, err
	}
	to, err := time.Parse("2006-01-02", w.To)
	if err != nil {
		return Compare{}, err
	}
	days := int(to.Sub(from).Hours()/24) + 1

	prev := Window{
		From:   from.AddDate(0, 0, -days).Format("2006-01-02"),
		To:     from.AddDate(0, 0, -1).Format("2006-01-02"),
		Person: w.Person,
	}
	prevTotals, err := d.Totals(ctx, prev)
	if err != nil {
		return Compare{}, err
	}
	return Compare{Current: cur, Previous: prevTotals, PreviousFrom: prev.From, PreviousTo: prev.To}, nil
}

// ExportRow is one row of the CSV download: a day's totals per dimension.
// Per-event rows would bury a spreadsheet in individual responses.
type ExportRow struct {
	Day          string
	Person       string
	Source       string
	Model        string
	Effort       string
	Origin       string
	Tokens       int64
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CostUSD      float64
	CostBasis    string
	Events       int64
	// UnpricedTokens is the part of Tokens no rate matched, so a $0 row
	// cannot pass for free usage.
	UnpricedTokens int64
}

func (d *DB) Export(ctx context.Context, w Window) ([]ExportRow, error) {
	w = w.Normalise()
	where, args := w.where("e.")
	rows, err := d.read.QueryContext(ctx, `
		SELECT e.day, COALESCE(NULLIF(a.email,''), e.account_ref), e.source, e.model,
		       e.effort, CASE WHEN e.is_subagent THEN 'subagent' ELSE 'main' END,
		       SUM(e.total_tokens), SUM(e.input_tokens), SUM(e.output_tokens),
		       SUM(e.cache_read_tokens), SUM(e.cost_usd), e.cost_basis, SUM(e.events),
		       SUM(CASE WHEN e.cost_source = 'unpriced' THEN e.total_tokens ELSE 0 END)
		FROM event_daily e LEFT JOIN account a ON a.ref = e.account_ref
		WHERE `+where+`
		GROUP BY 1,2,3,4,5,6,12 ORDER BY 1 DESC, 7 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ExportRow, 0)
	for rows.Next() {
		var r ExportRow
		if err := rows.Scan(&r.Day, &r.Person, &r.Source, &r.Model, &r.Effort, &r.Origin,
			&r.Tokens, &r.InputTokens, &r.OutputTokens, &r.CacheRead,
			&r.CostUSD, &r.CostBasis, &r.Events, &r.UnpricedTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MatrixCell is one row/column intersection of a two-dimensional breakdown.
type MatrixCell struct {
	Row    string `json:"row"`
	Col    string `json:"col"`
	Tokens int64  `json:"tokens"`
	// Three figures, never a sum: see Totals.
	BilledUSD       float64 `json:"billed_usd"`
	RateCardUSD     float64 `json:"rate_card_usd"`
	UnknownBasisUSD float64 `json:"unknown_basis_usd"`
	Events          int64   `json:"events"`
}

// ErrUnknownDimension marks a caller's mistake rather than a server fault, so
// the handler can answer 400 for it and keep everything else internal.
var ErrUnknownDimension = errors.New("unknown dimension")

// matrixDims is the allow-list. These reach SQL as expressions, so they are
// mapped through here rather than interpolated from the query string.
var matrixDims = map[string]string{
	"model":   "model",
	"effort":  effortKey("effort"),
	"source":  "source",
	"surface": "surface",
	"speed":   "speed",
}

// MatrixResult carries the cells plus the order their columns belong in. Only
// the server knows whether a dimension is ordinal; a client inferring the
// order from the data re-sorts an ordinal scale by volume.
type MatrixResult struct {
	Cells    []MatrixCell `json:"cells"`
	ColOrder []string     `json:"col_order,omitempty"`
}

// Matrix cross-tabulates two dimensions.
func (d *DB) Matrix(ctx context.Context, w Window, rows, cols string, limit int) (*MatrixResult, error) {
	rowCol, ok := matrixDims[rows]
	if !ok {
		return nil, fmt.Errorf("%w: row %q", ErrUnknownDimension, rows)
	}
	colCol, ok := matrixDims[cols]
	if !ok {
		return nil, fmt.Errorf("%w: column %q", ErrUnknownDimension, cols)
	}
	if limit <= 0 || limit > 12 {
		limit = 6
	}
	w = w.Normalise()
	where, args := w.where("")

	// The busiest rows only, so the chart keeps a readable number of bars --
	// except effort, an ordinal scale, drawn whole and in its own order.
	rowFilter, order := rowCol+" != ''", "3 DESC, 1, 2"
	qargs := append([]any{}, args...)
	if rows == "effort" {
		order = effortOrder(rowCol) + ", 3 DESC, 2"
	} else {
		rowFilter = fmt.Sprintf(`%[1]s IN (
		  SELECT %[1]s FROM event_daily WHERE %[2]s AND %[1]s != ''
		  GROUP BY 1 ORDER BY SUM(total_tokens) DESC LIMIT ?)`, rowCol, where)
		qargs = append(append(qargs, args...), limit)
	}
	q := fmt.Sprintf(`
		SELECT %[1]s, COALESCE(NULLIF(%[2]s,''), 'unknown'),
		       SUM(total_tokens),
		       `+billedUSD+`,
		       `+rateCardUSD+`,
		       `+unknownBasisUSD+`,
		       SUM(events)
		FROM event_daily
		WHERE %[3]s AND %[4]s
		GROUP BY 1, 2 ORDER BY %[5]s`, rowCol, colCol, where, rowFilter, order)

	rowsRes, err := d.read.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rowsRes.Close()

	res := &MatrixResult{Cells: make([]MatrixCell, 0)}
	for rowsRes.Next() {
		var c MatrixCell
		if err := rowsRes.Scan(&c.Row, &c.Col, &c.Tokens,
			&c.BilledUSD, &c.RateCardUSD, &c.UnknownBasisUSD, &c.Events); err != nil {
			return nil, err
		}
		res.Cells = append(res.Cells, c)
	}
	if err := rowsRes.Err(); err != nil {
		return nil, err
	}
	if cols == "effort" {
		// The display order, not the bare scale: see EffortDisplayOrder.
		res.ColOrder = schema.EffortDisplayOrder()
	}
	return res, nil
}
