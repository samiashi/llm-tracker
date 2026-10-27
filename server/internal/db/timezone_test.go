package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

func TestViewerTimezoneBucketsPreserveInstantsAndTotals(t *testing.T) {
	for _, tc := range []struct {
		name, zone        string
		instants, buckets []string
		hours             []int
	}{
		{"Dubai", "Asia/Dubai", []string{"2026-10-27T08:15:00Z"}, []string{"2026-10-27T08:00:00Z"}, []int{12}},
		{"half hour", "Asia/Kolkata", []string{"2026-10-27T08:15:00Z"}, []string{"2026-10-27T07:30:00Z"}, []int{13}},
		{"quarter hour", "Asia/Kathmandu", []string{"2026-10-27T08:15:00Z"}, []string{"2026-10-27T08:15:00Z"}, []int{14}},
		{"fall back", "America/New_York", []string{"2026-11-01T05:15:00Z", "2026-11-01T06:15:00Z"}, []string{"2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z"}, []int{1, 1}},
		{"spring forward", "America/New_York", []string{"2026-03-08T06:30:00Z", "2026-03-08T07:30:00Z"}, []string{"2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z"}, []int{1, 3}},
		{"midnight DST", "America/Santiago", []string{"2026-09-06T03:30:00Z", "2026-09-06T04:30:00Z"}, []string{"2026-09-06T03:00:00Z", "2026-09-06T04:00:00Z"}, []int{23, 1}},
		{"half-hour DST", "Australia/Lord_Howe", []string{"2026-10-03T15:29:00Z", "2026-10-03T15:30:00Z", "2026-10-03T16:00:00Z"}, []string{"2026-10-03T14:30:00Z", "2026-10-03T15:30:00Z", "2026-10-03T16:00:00Z"}, []int{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDB(t)
			ctx := context.Background()
			zone, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			var first time.Time
			for i, instant := range tc.instants {
				at, err := time.Parse(time.RFC3339, instant)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = at
				}
				e := ev(string(rune('a'+i)), 100, schema.CostBilled)
				e.TS = at
				ingest(t, d, e)
			}
			w := Window{Start: first.Add(-time.Hour).Unix(), End: first.Add(23 * time.Hour).Unix(), Zone: zone}
			rows, err := d.Daily(ctx, w)
			if err != nil || len(rows) != len(tc.buckets) {
				t.Fatalf("buckets %+v (%v)", rows, err)
			}
			var total int64
			for i, row := range rows {
				if row.Day != tc.buckets[i] {
					t.Errorf("bucket %s, want %s", row.Day, tc.buckets[i])
				}
				total += row.Totals.TotalTokens
			}
			models, err := d.DailyByModel(ctx, w, 8)
			if err != nil || len(models) != len(rows) {
				t.Fatalf("models: %+v (%v)", models, err)
			}
			for i, model := range models {
				if model.Day != rows[i].Day {
					t.Errorf("model bucket differs: %+v", model)
				}
			}
			heat, _, err := d.Heatmap(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			var heatTotal int64
			gotHours := map[int]int64{}
			for _, cell := range heat {
				heatTotal += cell.Tokens
				gotHours[cell.Hour] += cell.Tokens
			}
			wantHours := map[int]int64{}
			for _, hour := range tc.hours {
				wantHours[hour] += 100
			}
			for hour, n := range wantHours {
				if gotHours[hour] != n {
					t.Errorf("heat hour %d = %d, want %d", hour, gotHours[hour], n)
				}
			}
			totals, err := d.Totals(ctx, w)
			if err != nil || total != totals.TotalTokens || heatTotal != total {
				t.Fatalf("lost tokens: series %d, heat %d, totals %+v (%v)", total, heatTotal, totals, err)
			}
		})
	}
}
