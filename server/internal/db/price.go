package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// priceEvent resolves a cost and says where it came from.
//
// A harness that computes its own cost wins: opencode prices against the rates
// it was actually billed at, which is better information than our table has.
// When nothing resolves we record zero and mark it unpriced, so the dashboard
// can show unpriced volume rather than letting an unrecognised model look free.
func (d *DB) priceEvent(e *schema.Event) (float64, string) {
	if e.NativeCostUSD != nil {
		return *e.NativeCostUSD, "native"
	}
	if usd, ok := d.prices.Cost(e); ok {
		return usd, "table"
	}
	return 0, "unpriced"
}

// pricedColumns are the stored columns a table price is computed from, in the
// order pricedDest scans them. A cost that disagrees with them is wrong.
// source is one: long-context tiers apply only to sources that report one
// request per event (schema.Event.coversOneRequest).
const pricedColumns = "source, model, endpoint, speed, inference_geo, input_tokens, " +
	"output_tokens, cache_read_tokens, cache_write_5m, cache_write_1h, web_search_calls"

// selectPriced reads table-priced rows with the columns their cost prices.
const selectPriced = `SELECT id, cost_usd, cost_source, ` + pricedColumns +
	` FROM event WHERE cost_source != 'native'`

func pricedDest(e *schema.Event) []any {
	u := &e.Usage
	return []any{(*string)(&e.Source), &e.Model, &e.Endpoint, &e.Speed, &e.InferenceGeo,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWrite5mTokens,
		&u.CacheWrite1hTokens, &u.WebSearchCalls}
}

// scanStale reads one selectPriced row and reports whether its cost disagrees
// with its columns, returning the price it should have.
func (d *DB) scanStale(rows *sql.Rows) (r repricing, stale bool, err error) {
	if err := rows.Scan(append([]any{&r.id, &r.cost, &r.source}, pricedDest(&r.priced)...)...); err != nil {
		return r, false, err
	}
	cost, source := d.priceEvent(&r.priced)
	stale = cost != r.cost || source != r.source
	r.cost, r.source = cost, source
	return r, stale, nil
}

// pricedValues is pricedDest's values, for binding.
func pricedValues(e *schema.Event) []any {
	dest := pricedDest(e)
	out := make([]any, len(dest))
	for i, p := range dest {
		switch v := p.(type) {
		case *string:
			out[i] = *v
		case *int64:
			out[i] = *v
		}
	}
	return out
}

// repriceBatch bounds each write transaction reprice takes, so ingest waits
// milliseconds for the lock rather than the whole pass.
const repriceBatch = 1_000

// repricing is one row's new cost and the values it was priced from.
type repricing struct {
	id     string
	cost   float64
	source string
	priced schema.Event
}

// pricedByKey is the setting naming what stored costs were last priced by.
const pricedByKey = "priced_by"

// RepriceIfChanged reprices every stored event when the costs were last
// priced by anything but this build and its price table, then records them.
// Prices, and the code that applies them, change only with a release, so each
// new build reprices once; a pass cut short is repeated at the next start.
// ran reports whether it repriced at all.
func (d *DB) RepriceIfChanged(ctx context.Context, build string) (updated int, ran bool, err error) {
	pricedBy := build + " " + d.prices.Version
	var last string
	err = d.read.QueryRowContext(ctx,
		`SELECT value FROM setting WHERE key = ?`, pricedByKey).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if last == pricedBy {
		return 0, false, nil
	}
	if updated, err = d.reprice(ctx); err != nil {
		return updated, true, err
	}
	_, err = d.write.ExecContext(ctx,
		`INSERT INTO setting (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, pricedByKey, pricedBy)
	return updated, true, err
}

// reprice recomputes the cost of every event priced from the table, against
// the current price table, and returns how many changed. Cost is resolved at
// ingest, so this is how a corrected rate reaches rows already stored; native
// costs are left alone.
//
// It runs beside live ingest, so every write is conditional on the row still
// holding what was priced: a reading ingest merged in meanwhile was priced by
// ingest itself, and a native figure that arrived is never replaced by an
// estimate.
func (d *DB) reprice(ctx context.Context) (updated int, err error) {
	rows, err := d.read.QueryContext(ctx, selectPriced)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var batch []repricing
	flush := func() error {
		n, err := d.applyReprice(ctx, batch)
		updated += n
		batch = batch[:0]
		return err
	}
	for rows.Next() {
		r, stale, err := d.scanStale(rows)
		if err != nil {
			return updated, err
		}
		if !stale {
			continue
		}
		batch = append(batch, r)
		if len(batch) == repriceBatch {
			if err := flush(); err != nil {
				return updated, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return updated, err
	}
	return updated, flush()
}

// applyReprice writes each new cost where the row still holds the values it
// was priced from and is still not native, and returns how many it wrote.
func (d *DB) applyReprice(ctx context.Context, batch []repricing) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	tx, err := d.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE event SET cost_usd = ?, cost_source = ?
		 WHERE id = ? AND cost_source != 'native'
		   AND `+strings.ReplaceAll(pricedColumns, ", ", " = ? AND ")+` = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for i := range batch {
		r := &batch[i]
		res, err := stmt.ExecContext(ctx,
			append([]any{r.cost, r.source, r.id}, pricedValues(&r.priced)...)...)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// priceMerged prices each merged row from its stored columns, not from the
// reading merged into it: a merge keeps the larger tool-call counts and may
// keep the stored split, and a cost for columns not stored disagrees with the
// columns that are. Native figures are the harness's own, and are left alone.
func (d *DB) priceMerged(ctx context.Context, tx *sql.Tx, ids []string) error {
	// Read in chunks, then written: every Rows open in a transaction costs
	// database/sql a goroutine, and one query per row doubles ingest time.
	const chunk = 500
	for len(ids) > 0 {
		n := min(len(ids), chunk)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		ids = ids[n:]

		stale, err := func() ([]repricing, error) {
			rows, err := tx.QueryContext(ctx,
				selectPriced+` AND id IN (?`+strings.Repeat(", ?", n-1)+`)`, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []repricing
			for rows.Next() {
				r, stale, err := d.scanStale(rows)
				if err != nil {
					return nil, err
				}
				if stale {
					out = append(out, r)
				}
			}
			return out, rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, r := range stale {
			if _, err := tx.ExecContext(ctx,
				`UPDATE event SET cost_usd = ?, cost_source = ? WHERE id = ?`,
				r.cost, r.source, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}
