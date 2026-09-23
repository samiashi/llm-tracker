package db

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// Asked for more than a list holds, a caller gets as many as it may have, not
// the default: nine models asked for is eight drawn, never six.
func TestAListAskedPastItsCapGetsTheCap(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	w := Window{From: "2000-01-01"}
	var evs []schema.Event
	for i := range 101 {
		e := ev(fmt.Sprintf("e%d", i), int64(1_000+i), schema.CostBilled)
		e.SessionID, e.Model = fmt.Sprintf("s%d", i), fmt.Sprintf("model-%02d", i%14)
		evs = append(evs, e)
	}
	ingest(t, d, evs...)

	models := func(top int) int {
		t.Helper()
		pts, err := d.DailyByModel(ctx, w, top)
		if err != nil {
			t.Fatal(err)
		}
		var seen []string
		for _, p := range pts {
			if !slices.Contains(seen, p.Model) {
				seen = append(seen, p.Model)
			}
		}
		return len(seen)
	}
	rows := func(limit int) int {
		t.Helper()
		m, err := d.Matrix(ctx, w, "model", "effort", limit)
		if err != nil {
			t.Fatal(err)
		}
		return len(m.Cells) // one effort, so one cell per row
	}
	sessions := func(limit int) int {
		t.Helper()
		s, err := d.TopSessions(ctx, w, limit)
		if err != nil {
			t.Fatal(err)
		}
		return len(s)
	}
	for _, c := range []struct {
		list      string
		n         func(int) int
		asked     int
		wantCount int
	}{
		{"models", models, 0, 6}, {"models", models, 9, 8}, {"models", models, 3, 3},
		{"matrix rows", rows, 0, 6}, {"matrix rows", rows, 13, 12},
		{"sessions", sessions, 0, 10}, {"sessions", sessions, 101, 100},
	} {
		if got := c.n(c.asked); got != c.wantCount {
			t.Errorf("%s asked for %d: got %d, want %d", c.list, c.asked, got, c.wantCount)
		}
	}
}

// One origin, one name for it: the breakdown and the download label the same
// rows the same way, or a sheet built from one misreads the other.
func TestOriginHasOneLabelEverywhere(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	sub := ev("sub", 100, schema.CostBilled)
	sub.IsSubagent = true
	ingest(t, d, ev("main", 200, schema.CostBilled), sub)
	w := Window{From: "2000-01-01"}

	groups, err := d.Breakdown(ctx, w, "origin")
	if err != nil {
		t.Fatal(err)
	}
	var fromBreakdown []string
	for _, g := range groups {
		fromBreakdown = append(fromBreakdown, g.Key)
	}
	rows, err := d.Export(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	var fromExport []string
	for _, r := range rows {
		fromExport = append(fromExport, r.Origin)
	}
	slices.Sort(fromBreakdown)
	slices.Sort(fromExport)
	if want := []string{"main", "subagent"}; !slices.Equal(fromBreakdown, want) || !slices.Equal(fromExport, want) {
		t.Fatalf("origins: breakdown %v, export %v; want both %v", fromBreakdown, fromExport, want)
	}
}
