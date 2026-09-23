package db

import (
	"context"
	"strings"
	"testing"
)

// planStep is one row of EXPLAIN QUERY PLAN.
type planStep struct {
	id, parent int
	detail     string
}

func explain(t *testing.T, d *DB, q string) []planStep {
	t.Helper()
	rows, err := d.read.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []planStep
	for rows.Next() {
		var s planStep
		var unused int
		if err := rows.Scan(&s.id, &s.parent, &unused, &s.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Agents, SourceHealth and DayRange run on every poll over all of history, so
// each takes event's rows from an index already in the order it needs: a
// covering scan with no sort beside it, or a seek.
func TestPollQueriesNeverSortAllOfHistory(t *testing.T) {
	d := newDB(t)
	cases := []struct {
		name, query string
		seekOnly    bool
	}{
		{"agents", agentsQuery, false},
		{"source health", sourceHealthQuery, false},
		{"day range", dayRangeQuery, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, d, c.query)
			var dump strings.Builder
			for _, s := range plan {
				dump.WriteString("\n  " + s.detail)
			}
			read := false
			for _, s := range plan {
				if !strings.HasPrefix(s.detail, "SCAN event") &&
					!strings.HasPrefix(s.detail, "SEARCH event") {
					continue
				}
				read = true
				if !strings.Contains(s.detail, "COVERING INDEX") {
					t.Fatalf("event read row by row: %q%s", s.detail, dump.String())
				}
				if c.seekOnly && strings.HasPrefix(s.detail, "SCAN") {
					t.Fatalf("event scanned end to end for a MIN or MAX: %q%s", s.detail, dump.String())
				}
				for _, sib := range plan {
					if sib.parent == s.parent && strings.HasPrefix(sib.detail, "USE TEMP B-TREE FOR GROUP BY") {
						t.Fatalf("event's rows are sorted to be grouped%s", dump.String())
					}
				}
			}
			if !read {
				t.Fatalf("the plan never reads event; the query lost a branch%s", dump.String())
			}
		})
	}
}

// Invariant 5: the view must not pre-aggregate. Grouped inside the view, every
// windowed query sorts its rows into a temp B-tree before its own aggregate
// redoes the work -- 14x slower at a million events (see 00008). Totals has no
// GROUP BY of its own, so any grouping in its plan is the view's.
func TestTheViewHandsOverRowsUngrouped(t *testing.T) {
	d := newDB(t)
	var def string
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'event_daily'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(def), "GROUP BY") {
		t.Fatalf("event_daily aggregates at query time:\n%s", def)
	}
	for _, s := range explain(t, d, `SELECT `+totalsSelect+
		` FROM event_daily WHERE day BETWEEN '2000-01-01' AND '2099-01-01'`) {
		if strings.Contains(s.detail, "FOR GROUP BY") {
			t.Fatalf("event_daily groups its rows before the caller does: %q", s.detail)
		}
	}
}
