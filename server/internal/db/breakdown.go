package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// Breakdowns of a window by one or two dimensions, over event_daily.

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
			g.Key = unknownKey
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// breakdownDims is Breakdown's allow-list: the SQL for each dimension a
// request may name, so the query string never reaches the database.
var breakdownDims = map[string]struct{ expr, join, label string }{
	// By email rather than by account: one person holds several accounts, and
	// grouped by account_ref they would appear once per login with their usage
	// split across the rows.
	"person": {"COALESCE(NULLIF(account.email,''), event.account_ref)",
		"LEFT JOIN account ON account.ref = event.account_ref", ""},
	"model":   {"event.model", "", "event.provider"},
	"source":  {"event.source", "", "event.surface"},
	"surface": {"event.surface", "", ""},
	"origin":  {origin("event."), "", ""},
}

// origin separates main-thread work from subagent fan-out, under the labels
// the breakdown and the CSV share.
func origin(col string) string {
	return "CASE WHEN " + col + "is_subagent THEN 'subagent' ELSE 'main' END"
}

// Breakdown groups a window by one allow-listed dimension, largest first.
func (d *DB) Breakdown(ctx context.Context, w Window, by string) ([]Group, error) {
	dim, ok := breakdownDims[by]
	if !ok {
		return nil, unknownDimension("by", by, breakdownDims)
	}
	return d.breakdown(ctx, w, dim.expr, dim.join, dim.label)
}

// unknownDimension names the request parameter, the value it carried and the
// values it takes, which is everything the caller needs to correct it.
func unknownDimension[V any](param, got string, dims map[string]V) error {
	return fmt.Errorf("%w %q for %s; accepted: %s", ErrUnknownDimension, got, param,
		strings.Join(slices.Sorted(maps.Keys(dims)), ", "))
}

// There is no breakdown by project or branch. Both are high-cardinality and
// absent from daily_rollup, where they would multiply its rows, so event_daily
// cannot serve one, and one read from event under-reports every pruned day.

// effortOrder sorts by schema.EffortRank, then breaks a tie as
// EffortDisplayOrder does -- a level before the aliases that run at it -- then
// by value, so ultracode and xhigh never swap places between two loads.
func effortOrder(col string) string {
	display := schema.EffortDisplayOrder()
	var b strings.Builder
	b.WriteString(schema.EffortOrderSQL(col) + ", CASE " + schema.EffortKeySQL(col))
	for i, e := range display {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", e, i)
	}
	fmt.Fprintf(&b, " ELSE %d END, %s", len(display), col)
	return b.String()
}

// ModelDay is one model's tokens on one day.
type ModelDay struct {
	Day    string `json:"day"`
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// modelSeries is how many models DailyByModel draws.
var modelSeries = listLen{def: 6, max: 8}

// DailyByModel returns a per-day series for the busiest models. The rest are
// left out, not folded into an "other" band: the chart does not stack, so it
// never claims to cover every token, and the model breakdown beside it lists
// them all.
func (d *DB) DailyByModel(ctx context.Context, w Window, top int) ([]ModelDay, error) {
	w = w.Normalise()
	where, args := w.where("")
	q := `
		WITH ranked AS (
		  SELECT model, SUM(total_tokens) AS t FROM event_daily
		  WHERE ` + where + ` AND model != ''
		  GROUP BY model ORDER BY t DESC, model LIMIT ?
		)
		SELECT day, model, SUM(total_tokens)
		FROM event_daily WHERE ` + where + `
		  AND model IN (SELECT model FROM ranked)
		GROUP BY 1, 2 ORDER BY 1, 2`
	qargs := append(append([]any{}, args...), modelSeries.of(top))
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
	// Grouped by cost_basis as well (invariant 3): one cost_usd holds one
	// basis, never seat usage added to metered spend.
	rows, err := d.read.QueryContext(ctx, `
		SELECT e.day, COALESCE(NULLIF(a.email,''), e.account_ref), e.source, e.model,
		       e.effort, `+origin("e.")+`,
		       SUM(e.total_tokens), SUM(e.input_tokens), SUM(e.output_tokens),
		       SUM(e.cache_read_tokens), SUM(e.cost_usd), e.cost_basis, SUM(e.events),
		       SUM(CASE WHEN e.cost_source = 'unpriced' THEN e.total_tokens ELSE 0 END)
		FROM event_daily e LEFT JOIN account a ON a.ref = e.account_ref
		WHERE `+where+`
		GROUP BY 1,2,3,4,5,6,12 ORDER BY 1 DESC, 7 DESC, 2,3,4,5,6,12`, args...)
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
	"effort":  schema.EffortKeySQL("effort"),
	"source":  "source",
	"surface": "surface",
	"speed":   "speed",
}

// matrixRows is how many rows Matrix draws, effort aside.
var matrixRows = listLen{def: 6, max: 12}

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
		return nil, unknownDimension("rows", rows, matrixDims)
	}
	colCol, ok := matrixDims[cols]
	if !ok {
		return nil, unknownDimension("cols", cols, matrixDims)
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
		  GROUP BY 1 ORDER BY SUM(total_tokens) DESC, 1 LIMIT ?)`, rowCol, where)
		qargs = append(append(qargs, args...), matrixRows.of(limit))
	}
	q := fmt.Sprintf(`
		SELECT %[1]s, COALESCE(NULLIF(%[2]s,''), '`+unknownKey+`'),
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
