package db

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// efforts ingests one event per effort value, the first carrying the most
// tokens, so a volume sort and the scale disagree.
func efforts(t *testing.T, d *DB, values ...string) {
	t.Helper()
	var evs []schema.Event
	for i, v := range values {
		e := ev(fmt.Sprintf("e%d", i), int64(1_000*(len(values)-i)), schema.CostBilled)
		e.Effort = v
		evs = append(evs, e)
	}
	ingest(t, d, evs...)
}

// displayed filters EffortDisplayOrder to the values present.
func displayed(present ...string) []string {
	var out []string
	for _, e := range schema.EffortDisplayOrder() {
		if slices.Contains(present, e) {
			out = append(out, e)
		}
	}
	return out
}

// Invariant 7: "XHigh" and " high" are the levels they spell, and effort rows
// keep the whole scale in order rather than the six busiest.
func TestTheEffortMatrixKeepsTheWholeNormalisedScale(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	w := Window{From: "2000-01-01"}
	efforts(t, d, "ultra", "XHigh", " high", "default", "minimal", "low",
		"medium", "max", "ultracode", "xhigh", "high")
	levels := displayed("default", "minimal", "low", "medium", "high", "xhigh",
		"ultracode", "max", "ultra")

	m, err := d.Matrix(ctx, w, "model", "effort", 6)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, c := range m.Cells {
		if !slices.Contains(m.ColOrder, c.Col) {
			t.Fatalf("column %q is not on the scale the chart draws", c.Col)
		}
		cols = append(cols, c.Col)
	}
	slices.Sort(cols)
	if got, want := slices.Compact(cols), slices.Sorted(slices.Values(levels)); !slices.Equal(got, want) {
		t.Fatalf("columns %v, want one per level %v", got, want)
	}

	m, err = d.Matrix(ctx, w, "effort", "model", 6)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, c := range m.Cells {
		if len(rows) == 0 || rows[len(rows)-1] != c.Row {
			rows = append(rows, c.Row)
		}
	}
	if !slices.Equal(rows, levels) {
		t.Fatalf("effort rows %v, want the whole scale in order %v", rows, levels)
	}
}

// A tie on the scale -- ultracode runs at xhigh -- is broken the way the
// display order draws it, so two loads of one card never swap them.
func TestEffortRowsFollowTheDisplayOrder(t *testing.T) {
	d := newDB(t)
	efforts(t, d, "ultracode", "max", "XHigh", "high", "xhigh")

	for range 3 {
		m, err := d.Matrix(context.Background(), Window{From: "2000-01-01"}, "effort", "model", 6)
		if err != nil {
			t.Fatal(err)
		}
		var rows []string
		for _, c := range m.Cells {
			if len(rows) == 0 || rows[len(rows)-1] != c.Row {
				rows = append(rows, c.Row)
			}
		}
		if want := displayed("ultracode", "max", "xhigh", "high"); !slices.Equal(rows, want) {
			t.Fatalf("effort rows %v, want %v", rows, want)
		}
	}
}
