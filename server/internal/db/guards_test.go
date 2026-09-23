package db

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// Invariant 5: every windowed total reads the same before and after a prune.
// Pointed at event rather than event_daily, a query loses every pruned day. A
// method taking a Window must be listed here or among the exempt, so a new
// one cannot skip the check.
func TestEveryWindowedTotalSurvivesAPrune(t *testing.T) {
	ctx := context.Background()
	pruned := time.Now().AddDate(0, 0, -200).UTC()
	sumGroups := func(by string) func(d *DB, w Window) (int64, error) {
		return func(d *DB, w Window) (int64, error) {
			gs, err := d.Breakdown(ctx, w, by)
			var n int64
			for _, g := range gs {
				n += g.Totals.TotalTokens
			}
			return n, err
		}
	}
	covered := map[string]func(d *DB, w Window) (int64, error){
		"Totals": func(d *DB, w Window) (int64, error) {
			t, err := d.Totals(ctx, w)
			return t.TotalTokens, err
		},
		// The window after the old day, so the one it compares with holds it.
		"Compare": func(d *DB, _ Window) (int64, error) {
			from := pruned.AddDate(0, 0, 1).Format(time.DateOnly)
			c, err := d.Compare(ctx, Window{From: from, To: from})
			return c.Previous.TotalTokens, err
		},
		"Daily": func(d *DB, w Window) (int64, error) {
			days, err := d.Daily(ctx, w)
			var n int64
			for _, x := range days {
				n += x.Totals.TotalTokens
			}
			return n, err
		},
		"DailyByModel": func(d *DB, w Window) (int64, error) {
			pts, err := d.DailyByModel(ctx, w, 8)
			var n int64
			for _, p := range pts {
				n += p.Tokens
			}
			return n, err
		},
		"Matrix": func(d *DB, w Window) (int64, error) {
			m, err := d.Matrix(ctx, w, "model", "effort", 12)
			var n int64
			if m != nil {
				for _, c := range m.Cells {
					n += c.Tokens
				}
			}
			return n, err
		},
		"Export": func(d *DB, w Window) (int64, error) {
			rows, err := d.Export(ctx, w)
			var n int64
			for _, r := range rows {
				n += r.Tokens
			}
			return n, err
		},
	}
	for by := range breakdownDims {
		covered["Breakdown/"+by] = sumGroups(by)
	}
	// Exempt by AGENTS.md: each needs something a day-level rollup cannot hold.
	exempt := map[string]bool{"Heatmap": true, "TopSessions": true}

	checked := map[string]bool{}
	for name := range covered {
		method, _, _ := strings.Cut(name, "/")
		checked[method] = true
	}
	windowT := reflect.TypeFor[Window]()
	dbT := reflect.TypeFor[*DB]()
	for i := range dbT.NumMethod() {
		m := dbT.Method(i)
		for j := 1; j < m.Type.NumIn(); j++ {
			if m.Type.In(j) == windowT && !checked[m.Name] && !exempt[m.Name] {
				t.Errorf("%s takes a Window but is not checked against a pruned day here", m.Name)
			}
		}
	}

	d := newDB(t)
	old := make([]schema.Event, 2)
	for i := range old {
		old[i] = ev("old-"+string(rune('a'+i)), 1_000, schema.CostBilled)
		old[i].TS = pruned
	}
	recent := ev("recent", 500, schema.CostBilled)
	recent.TS = pruned.AddDate(0, 0, 1)
	ingest(t, d, append(old, recent)...)
	w := Window{From: "2000-01-01"}
	before := map[string]int64{}
	for name, f := range covered {
		n, err := f(d, w)
		if err != nil || n == 0 {
			t.Fatalf("setup: %s = %d (%v) before any prune", name, n, err)
		}
		before[name] = n
	}
	if _, err := d.Prune(ctx, pruned.AddDate(0, 0, 1).Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, d); n != 1 {
		t.Fatalf("setup: %d raw events after the prune, want the one after the old day", n)
	}
	for name, f := range covered {
		if n, err := f(d, w); err != nil || n != before[name] {
			t.Errorf("%s = %d (%v) after the prune, want %d: a pruned day went missing",
				name, n, err, before[name])
		}
	}
}

// Missing the rollups, history starts after the pruned days: the dashboard
// clamps every chart to that first day and calls the prior period unrecorded.
func TestHistoryStillStartsAtARolledUpDay(t *testing.T) {
	old := oldDay("old", 1, 1_000)
	day := old[0].TS.UTC().Format(time.DateOnly)
	today := time.Now().UTC().Format(time.DateOnly)
	for _, tc := range []struct {
		name      string
		recent    bool
		wantFirst string
		wantLast  string
	}{
		{"beside raw events", true, day, today},
		{"with every day rolled up", false, day, day},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDB(t)
			evs := old
			if tc.recent {
				evs = append(slices.Clone(old), ev("recent", 10, schema.CostBilled))
			}
			ingest(t, d, evs...)
			pruneOld(t, d)
			for _, person := range []string{"", "dev@example.com"} {
				first, last, err := d.DayRange(context.Background(), person)
				if err != nil || first != tc.wantFirst || last != tc.wantLast {
					t.Fatalf("history for %q: %s -> %s (%v), want %s -> %s",
						person, first, last, err, tc.wantFirst, tc.wantLast)
				}
			}
		})
	}
}

// Invariant 3 in the download: a row per cost basis, never one row whose
// cost_usd adds seat usage to metered spend under whichever basis SQLite
// happened to read last.
func TestTheExportNeverSumsSpendAcrossCostBases(t *testing.T) {
	d := newDB(t)
	price := func(e schema.Event) float64 {
		usd, ok := d.prices.Cost(&e)
		if !ok {
			t.Fatal("setup: the fixture model is unpriced")
		}
		return usd
	}
	billed := ev("billed", 1_000_000, schema.CostBilled)
	seat := ev("seat", 2_000_000, schema.CostRateCard)
	ingest(t, d, billed, seat)

	rows, err := d.Export(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.CostBasis] += r.CostUSD
	}
	if len(rows) != 2 || got["billed"] != price(billed) || got["rate_card_equivalent"] != price(seat) {
		t.Fatalf("export rows %+v, want one per basis: billed $%v, rate card $%v",
			rows, price(billed), price(seat))
	}
}

// Invariant 6: a dashboard read never waits for a write. The write pool has one
// connection; a read sent through it queues behind whatever ingest or a prune
// holds, which took reads to 30 seconds. Every exported method is either a read
// listed here, run while a write transaction is open, or a listed write.
func TestReadsNeverQueueBehindAWrite(t *testing.T) {
	d := newDB(t)
	ingest(t, d, ev("a", 100, schema.CostBilled))
	w := Window{From: "2000-01-01"}
	ignore := func(_ any, err error) error { return err }
	reads := map[string]func(ctx context.Context) error{
		"Totals":         func(ctx context.Context) error { return ignore(d.Totals(ctx, w)) },
		"Compare":        func(ctx context.Context) error { return ignore(d.Compare(ctx, w)) },
		"Breakdown":      func(ctx context.Context) error { return ignore(d.Breakdown(ctx, w, "person")) },
		"Daily":          func(ctx context.Context) error { return ignore(d.Daily(ctx, w)) },
		"DailyByModel":   func(ctx context.Context) error { return ignore(d.DailyByModel(ctx, w, 6)) },
		"Matrix":         func(ctx context.Context) error { return ignore(d.Matrix(ctx, w, "model", "effort", 6)) },
		"Export":         func(ctx context.Context) error { return ignore(d.Export(ctx, w)) },
		"TopSessions":    func(ctx context.Context) error { return ignore(d.TopSessions(ctx, w, 10)) },
		"Agents":         func(ctx context.Context) error { return ignore(d.Agents(ctx)) },
		"SourceHealth":   func(ctx context.Context) error { return ignore(d.SourceHealth(ctx)) },
		"UnknownSources": func(ctx context.Context) error { return ignore(d.UnknownSources(ctx)) },
		"EarliestRawDay": func(ctx context.Context) error { return ignore(d.EarliestRawDay(ctx)) },
		"RetentionFloor": func(ctx context.Context) error { return ignore(d.RetentionFloor(ctx)) },
		"RollupsBefore":  func(ctx context.Context) error { return ignore(d.RollupsBefore(ctx)) },
		"DetailFrom":     func(ctx context.Context) error { return ignore(d.DetailFrom(ctx)) },
		"Heatmap": func(ctx context.Context) error {
			_, _, err := d.Heatmap(ctx, w)
			return err
		},
		"DayRange": func(ctx context.Context) error {
			_, _, err := d.DayRange(ctx, "dev@example.com")
			return err
		},
		"TokenLogin": func(ctx context.Context) error {
			_, _, err := d.TokenLogin(ctx, "ctk_x")
			return err
		},
	}
	// Writes, and methods that touch neither pool.
	others := map[string]bool{"Ingest": true, "Prune": true, "RepriceIfChanged": true,
		"IssueToken": true, "RevokeTokens": true, "Close": true, "PriceTableVersion": true}
	dbT := reflect.TypeFor[*DB]()
	for i := range dbT.NumMethod() {
		if name := dbT.Method(i).Name; reads[name] == nil && !others[name] {
			t.Errorf("%s is neither a read checked here nor a listed write", name)
		}
	}

	// Holds the only write connection, as a prune or a long ingest does.
	tx, err := d.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	for name, read := range reads {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := read(ctx)
		cancel()
		if err != nil {
			t.Errorf("%s did not answer while a write was open (%v): it went through the write pool", name, err)
		}
	}
}
