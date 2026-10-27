package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

func TestRollingWindowUsesExactBoundsAcrossEveryView(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	// Cross Santiago's midnight DST transition; elapsed time still means 24h.
	end := time.Date(2026, 9, 6, 15, 37, 0, 0, time.UTC).Unix()
	start := end - 86400
	for i, ts := range []int64{start - 86400, start - 1, start, end - 1, end, end + 1} {
		e := ev(string(rune('a'+i)), 100, schema.CostBilled)
		e.TS = time.Unix(ts, 0)
		e.SessionID = "session"
		ingest(t, d, e)
	}
	w := Window{Start: start, End: end, Person: "dev@example.com"}
	check := func(name string, n int64, err error) {
		t.Helper()
		if err != nil || n != 200 {
			t.Errorf("%s = %d (%v), want 200", name, n, err)
		}
	}
	totals, err := d.Totals(ctx, w)
	check("totals", totals.TotalTokens, err)
	if totals.Events != 2 {
		t.Errorf("events = %d, want 2", totals.Events)
	}
	for by := range breakdownDims {
		rows, err := d.Breakdown(ctx, w, by)
		var n int64
		for _, row := range rows {
			n += row.Totals.TotalTokens
		}
		check("breakdown/"+by, n, err)
	}
	days, err := d.Daily(ctx, w)
	var n int64
	for _, row := range days {
		n += row.Totals.TotalTokens
	}
	check("daily", n, err)
	models, err := d.DailyByModel(ctx, w, 8)
	n = 0
	for _, row := range models {
		n += row.Tokens
	}
	check("models", n, err)
	matrix, err := d.Matrix(ctx, w, "model", "effort", 12)
	n = 0
	if matrix != nil {
		for _, row := range matrix.Cells {
			n += row.Tokens
		}
	}
	check("matrix", n, err)
	export, err := d.Export(ctx, w)
	n = 0
	for _, row := range export {
		n += row.Tokens
	}
	check("export", n, err)
	heat, _, err := d.Heatmap(ctx, w)
	n = 0
	for _, row := range heat {
		n += row.Tokens
	}
	check("heatmap", n, err)
	sessions, err := d.TopSessions(ctx, w, 10)
	n = 0
	for _, row := range sessions {
		n += row.Tokens
	}
	check("sessions", n, err)
	compare, err := d.Compare(ctx, w)
	check("previous 24 hours", compare.Previous.TotalTokens, err)
	if compare.PreviousTo != time.Unix(start, 0).UTC().Format(time.RFC3339) {
		t.Errorf("comparison ends at %s", compare.PreviousTo)
	}
	w.Person = "nobody@example.com"
	totals, err = d.Totals(ctx, w)
	if err != nil || totals.TotalTokens != 0 {
		t.Fatalf("person filter: %+v (%v)", totals, err)
	}
}

func TestRollingSeriesGroupsByUTCHourAndPreservesTotals(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 5, 15, 37, 0, 0, time.UTC)
	offsets := []time.Duration{-time.Second, 0, time.Minute, 24 * time.Minute, 23 * time.Hour, 24 * time.Hour}
	for i, offset := range offsets {
		e := ev(string(rune('a'+i)), 100, schema.CostBilled)
		e.TS = start.Add(offset)
		ingest(t, d, e)
	}
	w := Window{Start: start.Unix(), End: start.Add(24 * time.Hour).Unix()}
	want := map[string]int64{"2026-09-05T15:00:00Z": 200, "2026-09-05T16:00:00Z": 100, "2026-09-06T14:00:00Z": 100}
	days, err := d.Daily(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != len(want) {
		t.Fatalf("hourly buckets: %+v", days)
	}
	for _, day := range days {
		if day.Totals.TotalTokens != want[day.Day] {
			t.Errorf("hour %+v, want %d", day, want[day.Day])
		}
	}
	models, err := d.DailyByModel(ctx, w, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != len(want) {
		t.Fatalf("model buckets: %+v", models)
	}
	for _, point := range models {
		if point.Tokens != want[point.Day] {
			t.Errorf("model hour %+v, want %d", point, want[point.Day])
		}
	}
	totals, err := d.Totals(ctx, w)
	if err != nil || totals.TotalTokens != 400 {
		t.Fatalf("totals: %+v (%v)", totals, err)
	}
	days, err = d.Daily(ctx, Window{From: "2026-09-05", To: "2026-09-06"})
	if err != nil || len(days) != 2 || days[0].Day != "2026-09-05" {
		t.Fatalf("daily range changed: %+v (%v)", days, err)
	}
}
