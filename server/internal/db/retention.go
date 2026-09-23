package db

import (
	"context"
	"database/sql"
	"strings"
)

// PruneResult reports what a retention pass did.
type PruneResult struct {
	Cutoff     string
	DaysRolled int
	// MachinesPruned counts agents that stopped reporting before the window.
	MachinesPruned int64
	RollupRows     int64
	EventsPruned   int64
	// Vacuumed reports whether the file was rebuilt to return its free pages
	// to the disk, and VacuumErr why that failed, after the prune committed.
	Vacuumed  bool
	VacuumErr error
}

// vacuumShare is how much of the file must be free pages before a prune
// rebuilds it. VACUUM copies the whole database under the write lock, which
// ingest waits on; a nightly prune frees a day's rows, which later inserts
// reuse, where the first prune of a long history frees most of the file.
const vacuumShare = 0.25

// rollupKey is daily_rollup's primary key: every dimension a rolled-up day
// keeps, and what Prune groups by. The two must agree or every prune aborts
// on the key (see 00010), so a column the table gains is added here too;
// TestRollupColumnsMatchTheSchema fails until it is.
const rollupKey = "day, machine_id, account_ref, source, surface, provider, model, endpoint, " +
	"effort, speed, inference_geo, is_subagent, cost_basis, cost_source"

// rollupMeasures are what a rolled-up day sums, besides its event count.
var rollupMeasures = []string{"input_tokens", "output_tokens", "cache_read_tokens",
	"cache_write_5m", "cache_write_1h", "reasoning_tokens",
	"web_search_calls", "web_fetch_calls", "total_tokens", "cost_usd"}

// sumsOf renders SUM(c) AS c for each column.
func sumsOf(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = "SUM(" + c + ") AS " + c
	}
	return strings.Join(out, ", ")
}

const (
	// retentionFloorKey is the day before which raw events were rolled up.
	retentionFloorKey = "retention_floor"
	// legacyFloorKey is the day before which rollups predate pruned_event and
	// so hold no ids: nothing arriving for those days can be told apart from
	// what they already count, so ingest refuses it.
	legacyFloorKey = "legacy_rollup_floor"
)

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
	// Their unknown sources go with them: only a complete report from the
	// machine replaces those, and a forgotten machine sends none.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM unknown_source
		 WHERE NOT EXISTS (SELECT 1 FROM machine WHERE id = unknown_source.machine_id)`); err != nil {
		return nil, err
	}

	// Nothing rolled up means the floor does not move. A floor on an empty
	// database would make a new server refuse the backfill it is about to be
	// sent.
	if res.DaysRolled == 0 {
		return res, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE merged AS
		SELECT `+rollupKey+`, SUM(events) AS events, `+sumsOf(rollupMeasures)+`
		FROM (
		  SELECT `+rollupKey+`, 1 AS events, `+strings.Join(rollupMeasures, ", ")+`
		  FROM event WHERE day < ?
		  UNION ALL
		  SELECT `+rollupKey+`, events, `+strings.Join(rollupMeasures, ", ")+`
		  FROM daily_rollup
		  WHERE day IN (SELECT DISTINCT day FROM event WHERE day < ?)
		)
		GROUP BY `+rollupKey,
		before, before); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM daily_rollup WHERE day IN (SELECT DISTINCT day FROM merged)`); err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `
		INSERT INTO daily_rollup (`+rollupKey+`, events, `+strings.Join(rollupMeasures, ", ")+`)
		SELECT * FROM merged`)
	if err != nil {
		return nil, err
	}
	res.RollupRows, _ = r.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DROP TABLE merged`); err != nil {
		return nil, err
	}

	// A row on a key its collector has since replaced comes back under a new
	// id once its machine upgrades, which the ledger cannot match: it would
	// count again beside this rollup. Such a day closes for good instead.
	// Codex's old keys go per machine when it first reports the new one
	// (00016); Cline and Roo Code re-keyed at collector 5, opencode at 7 and
	// Continue at 10.
	var stale bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM event WHERE day < ? AND (
		     (source = 'codex' AND native_id GLOB '*.jsonl#*'
		      AND machine_id NOT IN (SELECT machine_id FROM codex_rekeyed_machine))
		  OR (source IN ('cline', 'roo_code') AND collector < 5
		      AND native_id GLOB '*#*' AND native_id NOT GLOB '*#*#*')
		  OR (source = 'opencode' AND collector < 7)
		  OR (source = 'continue' AND collector < 10)))`,
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
	res.Vacuumed, res.VacuumErr = d.vacuumIfMostlyFree(ctx)
	return res, nil
}

// vacuumIfMostlyFree returns the file's free pages to the disk when they are
// vacuumShare of it or more: a DELETE alone leaves the file its old size.
func (d *DB) vacuumIfMostlyFree(ctx context.Context) (bool, error) {
	var free, pages int64
	if err := d.read.QueryRowContext(ctx, `
		SELECT f.freelist_count, p.page_count
		FROM pragma_freelist_count AS f, pragma_page_count AS p`).Scan(&free, &pages); err != nil {
		return false, err
	}
	if float64(free) < vacuumShare*float64(pages) {
		return false, nil
	}
	// VACUUM builds its copy of the file in the temp store, which the write
	// pool keeps in memory for ingest: the whole database in RAM on a small
	// VM. It copies to a file instead, and hands the connection back as it was.
	conn, err := d.write.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA temp_store = FILE`); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA temp_store = MEMORY`) }()
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return false, err
	}
	return true, nil
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

// ingestFloorTx is the day ingest refuses events below, or "" for none: the
// retention floor while this server prunes, and never below the legacy floor,
// whose rollups hold no ids to tell a re-send from a new event.
func (d *DB) ingestFloorTx(ctx context.Context, tx *sql.Tx) (string, error) {
	legacy, err := settingTx(ctx, tx, legacyFloorKey)
	if err != nil || !d.Pruning {
		return legacy, err
	}
	floor, err := settingTx(ctx, tx, retentionFloorKey)
	return max(floor, legacy), err
}

// DetailFrom is the first day with per-event detail, or "" if nothing was
// ever pruned. Days before it survive only as daily rollups, with no hours to
// show, and the dashboard labels them rather than drawing them as idle. The
// floor alone never moves back, so it would go on labelling days that agents
// have since re-delivered.
func (d *DB) DetailFrom(ctx context.Context) (string, error) {
	floor, err := d.RetentionFloor(ctx)
	if err != nil || floor == "" {
		return "", err
	}
	first, err := d.EarliestRawDay(ctx)
	if err != nil {
		return "", err
	}
	if first != "" && first < floor {
		return first, nil
	}
	return floor, nil
}
