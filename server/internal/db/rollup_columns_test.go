package db

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Prune's GROUP BY, the rollup table's key and the view every aggregate reads
// must name the same columns: a migration that widens one alone aborts every
// prune, as 00010 records.
func TestRollupColumnsMatchTheSchema(t *testing.T) {
	d := newDB(t)
	key := strings.Split(rollupKey, ", ")
	all := append(append(slices.Clone(key), "events"), rollupMeasures...)

	type col struct {
		name string
		pk   int
	}
	columns := func(table string) []col {
		rows, err := d.read.QueryContext(context.Background(),
			`SELECT name, pk FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []col
		for rows.Next() {
			var c col
			if err := rows.Scan(&c.name, &c.pk); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	var names, pk []string
	for _, c := range columns("daily_rollup") {
		names = append(names, c.name)
		if c.pk > 0 {
			pk = append(pk, c.name)
		}
	}
	if !slices.Equal(names, all) {
		t.Errorf("daily_rollup columns %v, Prune writes %v", names, all)
	}
	if !slices.Equal(pk, key) {
		t.Errorf("daily_rollup key %v, Prune groups by %v", pk, key)
	}

	var view []string
	for _, c := range columns("event_daily") {
		view = append(view, c.name)
	}
	if !slices.Equal(view, all) {
		t.Errorf("event_daily columns %v, the rollup holds %v", view, all)
	}
}
