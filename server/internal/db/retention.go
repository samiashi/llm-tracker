package db

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"
)

// PruneResult reports what a retention pass did.
type PruneResult struct {
	Cutoff     string `json:"cutoff"`
	DaysRolled int    `json:"days_rolled"`
	// MachinesPruned counts agents that stopped reporting before the window.
	MachinesPruned int64 `json:"machines_pruned"`
	RollupRows     int64 `json:"rollup_rows"`
	EventsPruned   int64 `json:"events_pruned"`
}

const (
	// retentionFloorKey is the day before which raw events were rolled up.
	retentionFloorKey = "retention_floor"
	// legacyFloorKey is the day before which rollups predate pruned_event and
	// so hold no ids: nothing arriving for those days can be told apart from
	// what they already count, so ingest refuses it.
	legacyFloorKey = "legacy_rollup_floor"
)

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

// RetentionFloor returns the day before which a prune deleted raw events, or
// "" if nothing has been pruned.
func (d *DB) RetentionFloor(ctx context.Context) (string, error) {
	var v string
	err := d.read.QueryRowContext(ctx,
		`SELECT value FROM setting WHERE key = ?`, retentionFloorKey).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// Prune rolls up every day before the cutoff and deletes those raw events.
//
// The rollup is built from the existing rollup plus the raw events, so a day
// that gained late events since it was last pruned accumulates rather than
// being replaced by an aggregate of the stragglers.
//
// Each id rolled up is recorded in pruned_event before its row is deleted, in
// the same transaction: it is what makes a later re-send of that event a
// no-op rather than a second count.
func (d *DB) Prune(ctx context.Context, before string) (*PruneResult, error) {
	res := &PruneResult{Cutoff: before}

	tx, err := d.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT day) FROM event WHERE day < ?`, before).Scan(&res.DaysRolled); err != nil {
		return nil, err
	}
	// Machines that stopped reporting before the window are forgotten, or
	// the agents table fills with decommissioned laptops. Keyed on last_seen,
	// so a machine still checking in is kept however quiet it is.
	m, err := tx.ExecContext(ctx,
		`DELETE FROM machine WHERE last_seen < ?`,
		mustDayUnix(before))
	if err != nil {
		return nil, err
	}
	res.MachinesPruned, _ = m.RowsAffected()

	// Nothing rolled up means the floor does not move. A floor on an empty
	// database would make a new server refuse the backfill it is about to be
	// sent.
	if res.DaysRolled == 0 {
		return res, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE merged AS
		SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
		       effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
		       SUM(events) AS events, SUM(input_tokens) AS input_tokens,
		       SUM(output_tokens) AS output_tokens, SUM(cache_read_tokens) AS cache_read_tokens,
		       SUM(cache_write_5m) AS cache_write_5m, SUM(cache_write_1h) AS cache_write_1h,
		       SUM(reasoning_tokens) AS reasoning_tokens,
		       SUM(web_search_calls) AS web_search_calls, SUM(web_fetch_calls) AS web_fetch_calls,
		       SUM(total_tokens) AS total_tokens, SUM(cost_usd) AS cost_usd
		FROM (
		  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
		         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
		         1 AS events, input_tokens, output_tokens, cache_read_tokens,
		         cache_write_5m, cache_write_1h, reasoning_tokens,
		         web_search_calls, web_fetch_calls, total_tokens, cost_usd
		  FROM event WHERE day < ?
		  UNION ALL
		  SELECT day, machine_id, account_ref, source, surface, provider, model, endpoint,
		         effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
		         events, input_tokens, output_tokens, cache_read_tokens,
		         cache_write_5m, cache_write_1h, reasoning_tokens,
		         web_search_calls, web_fetch_calls, total_tokens, cost_usd
		  FROM daily_rollup
		  WHERE day IN (SELECT DISTINCT day FROM event WHERE day < ?)
		)
		GROUP BY day, machine_id, account_ref, source, surface, provider, model, endpoint,
		         effort, speed, inference_geo, is_subagent, cost_basis, cost_source`,
		before, before); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM daily_rollup WHERE day IN (SELECT DISTINCT day FROM merged)`); err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `
		INSERT INTO daily_rollup (
			day, machine_id, account_ref, source, surface, provider, model, endpoint,
			effort, speed, inference_geo, is_subagent, cost_basis, cost_source,
			events, input_tokens, output_tokens, cache_read_tokens,
			cache_write_5m, cache_write_1h, reasoning_tokens,
			web_search_calls, web_fetch_calls, total_tokens, cost_usd)
		SELECT * FROM merged`)
	if err != nil {
		return nil, err
	}
	res.RollupRows, _ = r.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DROP TABLE merged`); err != nil {
		return nil, err
	}

	// Codex rows still on the ordinal key come back under content keys once
	// their machine upgrades (see 00016), which the ledger cannot match: those
	// would count again beside this rollup. Their days close for good instead.
	var stale bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM event
		  WHERE day < ? AND source = 'codex' AND native_id GLOB '*.jsonl#*'
		    AND machine_id NOT IN (SELECT machine_id FROM codex_rekeyed_machine))`,
		before).Scan(&stale); err != nil {
		return nil, err
	}
	if stale {
		if err := raiseFloor(ctx, tx, legacyFloorKey, before); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO pruned_event (id) SELECT id FROM event WHERE day < ?`,
		before); err != nil {
		return nil, err
	}
	d2, err := tx.ExecContext(ctx, `DELETE FROM event WHERE day < ?`, before)
	if err != nil {
		return nil, err
	}
	res.EventsPruned, _ = d2.RowsAffected()

	if err := raiseFloor(ctx, tx, retentionFloorKey, before); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// A DELETE alone leaves the file its old size; VACUUM returns the pages.
	_, _ = d.write.ExecContext(ctx, `VACUUM`)
	return res, nil
}

// raiseFloor moves a floor forward, never back: it records how far raw rows
// were deleted, and widening retention later does not bring them back. MAX in
// SQL keeps that true against a concurrent prune; ISO days sort
// chronologically.
func raiseFloor(ctx context.Context, tx *sql.Tx, key, before string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO setting (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = MAX(excluded.value, setting.value)`,
		key, before)
	return err
}
