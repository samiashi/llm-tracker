package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// In a zone behind UTC, an event early on the range's first UTC day falls on
// the previous local day and must still be counted. The clock is pinned:
// relative to time.Now(), the case arises only a few hours a day.
func TestHeatmapCoversTheLeftEdgeOfItsRange(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// 01:00 UTC on a fixed day -> previous local day in Santiago (UTC-3/-4).
	early := ev("edge-early", 1_000, schema.CostBilled)
	early.TS = time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	late := ev("edge-late", 1_000, schema.CostBilled)
	late.TS = time.Date(2026, 6, 15, 18, 0, 0, 0, time.UTC)
	ingest(t, d, early, late)

	cells, _, err := d.Heatmap(ctx, Window{From: "2000-01-01", To: "2026-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	var heat int64
	for _, c := range cells {
		heat += c.Tokens
	}
	tot, err := d.Totals(ctx, Window{From: "2000-01-01", To: "2026-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	if heat != tot.TotalTokens {
		t.Fatalf("heatmap = %d, totals = %d -- the heatmap must cover the same "+
			"events the headline does", heat, tot.TotalTokens)
	}
}

// The dashboard names the heatmap's zone from the offset Heatmap reports, so
// that offset must be the one the hours were bucketed with: an event lands in
// the hour its UTC time reaches at that offset, in any server zone.
func TestHeatmapReportsTheOffsetItsHoursUse(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	at := time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	e := ev("offset", 1_000, schema.CostBilled)
	e.TS = at
	ingest(t, d, e)

	cells, offset, err := d.Heatmap(ctx, Window{From: "2026-06-15", To: "2026-06-15"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("cells = %+v, want one", cells)
	}
	local := at.Add(time.Duration(offset) * time.Minute)
	if cells[0].Hour != local.Hour() || cells[0].Day != local.Format("2006-01-02") {
		t.Fatalf("cell %s %02d:00, but the reported offset %d min puts %s at %s",
			cells[0].Day, cells[0].Hour, offset, at.Format(time.RFC3339), local.Format("2006-01-02 15:04"))
	}
	if _, want := at.In(time.Local).Zone(); offset != want/60 {
		t.Fatalf("offset = %d min, want the server's own %d", offset, want/60)
	}
}
