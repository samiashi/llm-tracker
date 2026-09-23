package db

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// summaryDrift lists every key where event_day disagrees with a GROUP BY over
// event, which is what it summarises. Costs are compared to a nanodollar: the
// summary adds and subtracts them one reading at a time.
func summaryDrift(t *testing.T, d *DB) []string {
	t.Helper()
	measures := append([]string{"events"}, rollupMeasures...)
	summed := make([]string, len(measures))
	summed[0] = "COUNT(*)"
	for i, m := range rollupMeasures {
		summed[i+1] = "SUM(" + m + ")"
	}
	read := func(q string) map[string][]float64 {
		t.Helper()
		rows, err := d.read.QueryContext(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		nKey := len(strings.Split(rollupKey, ", "))
		out := map[string][]float64{}
		for rows.Next() {
			vals := make([]any, nKey+len(measures))
			dest := make([]any, len(vals))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprint(vals[:nKey]...)
			nums := make([]float64, len(measures))
			for i, v := range vals[nKey:] {
				switch n := v.(type) {
				case int64:
					nums[i] = float64(n)
				case float64:
					nums[i] = n
				}
			}
			out[key] = nums
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := read(`SELECT ` + rollupKey + `, ` + strings.Join(summed, ", ") +
		` FROM event GROUP BY ` + rollupKey)
	got := read(`SELECT ` + rollupKey + `, ` + strings.Join(measures, ", ") + ` FROM event_day`)

	var drift []string
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			drift = append(drift, fmt.Sprintf("missing %s: events hold %v", key, w))
			continue
		}
		for i := range w {
			if math.Abs(g[i]-w[i]) > 1e-9 {
				drift = append(drift, fmt.Sprintf("%s: %s = %v, events hold %v", key, measures[i], g[i], w[i]))
			}
		}
	}
	for key, g := range got {
		if _, ok := want[key]; !ok {
			drift = append(drift, fmt.Sprintf("stray %s: %v with no event behind it", key, g))
		}
	}
	return drift
}

// checkSummary fails the test if event_day has drifted from event. newDB runs
// it after every test in this package, so each one's writes are checked too.
func checkSummary(t *testing.T, d *DB) {
	t.Helper()
	if drift := summaryDrift(t, d); len(drift) > 0 {
		t.Errorf("event_day no longer sums event:\n  %s", strings.Join(drift, "\n  "))
	}
}

// fireFirst re-creates every trigger on event but event_day's, so they are
// newer than event_day's and SQLite runs them in the other order.
func fireFirst(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	defs := func() map[string]string {
		rows, err := d.write.QueryContext(ctx, `SELECT name, sql FROM sqlite_master
			WHERE type = 'trigger' AND tbl_name = 'event' AND name NOT LIKE 'event_day_%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatal(err)
			}
			out[name] = def
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}()
	if len(defs) == 0 {
		t.Fatal("setup: no re-key triggers on event to reorder")
	}
	for name, def := range defs {
		if _, err := d.write.ExecContext(ctx, `DROP TRIGGER `+name); err != nil {
			t.Fatal(err)
		}
		if _, err := d.write.ExecContext(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
}

// event_day stands in for event in every windowed query, so after any write
// it must equal a GROUP BY over event -- including the re-key triggers that
// delete a row inside the INSERT that stored it, in whichever order SQLite
// fires them beside event_day's own.
func TestTheDaySummaryAlwaysMatchesItsEvents(t *testing.T) {
	for _, order := range []string{"as migrated", "re-key triggers first"} {
		t.Run(order, func(t *testing.T) {
			d := newDB(t)
			if order != "as migrated" {
				fireFirst(t, d)
			}
			ctx := context.Background()
			step := func(name string, do func()) {
				t.Helper()
				do()
				if drift := summaryDrift(t, d); len(drift) > 0 {
					t.Fatalf("after %s, event_day no longer sums event:\n  %s", name, strings.Join(drift, "\n  "))
				}
			}
			at := func(e schema.Event, daysAgo int) schema.Event {
				e.TS = time.Now().AddDate(0, 0, -daysAgo)
				return e
			}
			unpriced := ev("unpriced", 3_000, schema.CostBilled)
			unpriced.Model = "not-a-real-model"
			seat := ev("seat", 2_000, schema.CostRateCard)
			seat.Effort, seat.Surface = "high", schema.SurfaceCLI

			step("new events", func() {
				ingest(t, d, ev("a", 1_000, schema.CostBilled), seat, unpriced,
					at(ev("old-1", 500, schema.CostBilled), 200), at(ev("old-2", 700, schema.CostRateCard), 200),
					at(ev("month", 900, schema.CostUnknown), 40))
			})
			step("a longer reading", func() { ingest(t, d, ev("a", 1_500, schema.CostBilled)) })
			step("a re-send", func() { ingest(t, d, ev("a", 1_500, schema.CostBilled), seat) })
			step("a newer collector moving the row's day, model, effort and surface", func() {
				moved := at(seat, 3)
				moved.Collector, moved.Model, moved.Effort, moved.Surface = 9, "claude-sonnet-5", "low", schema.SurfaceIDE
				ingest(t, d, moved)
			})
			step("a native cost", func() {
				native := ev("a", 1_500, schema.CostBilled)
				usd := 0.25
				native.NativeCostUSD = &usd
				ingest(t, d, native)
			})
			step("a merge inside the batch that inserted the row", func() {
				ingest(t, d, ev("fresh", 100, schema.CostBilled), ev("fresh", 400, schema.CostBilled))
			})
			step("an account and a basis filled in", func() {
				if _, err := d.Ingest(context.Background(), &schema.Batch{V: schema.Version, MachineID: "m2",
					Events: []schema.Event{func() schema.Event {
						e := ev("orphan", 50, schema.CostUnknown)
						e.MachineID, e.AccountRef = "m2", ""
						return e
					}()}}); err != nil {
					t.Fatal(err)
				}
				e := ev("orphan", 50, schema.CostRateCard)
				e.MachineID = "m2"
				ingest(t, d, e)
			})
			step("a reprice at a new rate, and of an unpriced model the table learns", func() {
				r := d.prices.Rates["|claude-opus-5"]
				r.Input *= 2
				d.prices.Rates["|claude-opus-5"] = r
				d.prices.Rates[schema.PriceKey("", "not-a-real-model")] = schema.Rate{Prices: schema.Prices{Input: 1}}
				if _, err := d.reprice(ctx); err != nil {
					t.Fatal(err)
				}
			})
			step("a prune", func() { pruneOld(t, d) })
			step("codex rows retired by a machine's first content key", func() {
				ingest(t, d,
					keyed(schema.SourceCodex, "m", "rollout-a.jsonl#1", "", rekeyAt, 100),
					keyed(schema.SourceCodex, "m", "rollout-a.jsonl#2", "", rekeyAt, 200))
				ingest(t, d, keyed(schema.SourceCodex, "m", "tc:first", "s", rekeyAt, 300))
			})
			step("a codex straggler refused inside its own insert", func() {
				ingest(t, d, keyed(schema.SourceCodex, "m", "rollout-b.jsonl#9", "", rekeyAt, 70))
			})
			step("one refused on a day no other event holds", func() {
				ingest(t, d, keyed(schema.SourceCodex, "m", "rollout-c.jsonl#3", "", rekeyAt.AddDate(0, 0, -1), 70))
			})
			step("cline index keys retired, then one refused", func() {
				ingest(t, d, keyed(schema.SourceCline, "m", "task#4", "task", rekeyAt, 10))
				ingest(t, d, keyed(schema.SourceCline, "m", "task#1790000001000#0", "task", rekeyAt, 10))
				ingest(t, d, keyed(schema.SourceCline, "m", "task#7", "task", rekeyAt, 10))
			})
			step("continue keys retired, then one refused", func() {
				ingest(t, d, keyed(schema.SourceContinue, "m", "tokensGenerated.jsonl#0", "", rekeyAt, 10))
				ingest(t, d, keyed(schema.SourceContinue, "m", "0.2.0/tokensGenerated.jsonl#0", "", rekeyAt, 10))
				ingest(t, d, keyed(schema.SourceContinue, "m",
					"/Users/dev/.continue/dev_data/0.2.0/tokensGenerated.jsonl#20260920T100000.000#m", "", rekeyAt, 10))
			})
		})
	}
}
