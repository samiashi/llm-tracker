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

func explain(t *testing.T, d *DB, q string, args ...any) []planStep {
	t.Helper()
	rows, err := d.read.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
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

// reads names the table a plan step reads, or "" for a step that reads none.
func (s planStep) reads() string {
	for _, verb := range []string{"SCAN ", "SEARCH "} {
		if rest, ok := strings.CutPrefix(s.detail, verb); ok {
			table, _, _ := strings.Cut(rest, " ")
			return table
		}
	}
	return ""
}

// Every poll runs these over all of history, and the windowed queries through
// the view, so none may read event row by row: that re-reads every stored
// event on every poll. Each reads the day summary instead, and touches event,
// if at all, only by a seek down a covering index.
func TestPollQueriesNeverReadEventRowByRow(t *testing.T) {
	d := newDB(t)
	cases := []struct {
		name  string
		query string
		args  []any
		// tables the plan must read, so a query cannot pass by losing a branch
		reads []string
	}{
		{"agents", agentsQuery, nil, []string{"event_day", "daily_rollup"}},
		{"source health", sourceHealthQuery, nil, []string{"event_day"}},
		{"day range", dayRangeQuery, nil, []string{"event_day", "daily_rollup"}},
		{"the view every windowed query reads",
			`SELECT ` + totalsSelect + ` FROM event_daily WHERE day BETWEEN ? AND ?`,
			[]any{"2026-01-01", "2026-01-31"}, []string{"event_day", "daily_rollup"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, d, c.query, c.args...)
			var dump strings.Builder
			for _, s := range plan {
				dump.WriteString("\n  " + s.detail)
			}
			read := map[string]bool{}
			for _, s := range plan {
				table := s.reads()
				read[table] = true
				if table != "event" {
					continue
				}
				if !strings.HasPrefix(s.detail, "SEARCH event USING COVERING INDEX") {
					t.Fatalf("event read row by row: %q%s", s.detail, dump.String())
				}
			}
			for _, want := range c.reads {
				if !read[want] {
					t.Fatalf("the plan never reads %s; the query lost a branch%s", want, dump.String())
				}
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
