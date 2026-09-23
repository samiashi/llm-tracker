package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// A session split evenly between two models or two efforts names the one it
// used last, not whichever the sort met first, or it swaps between polls.
func TestATiedSessionNamesTheModelAndEffortItUsedLast(t *testing.T) {
	d := newDB(t)
	at := time.Now().Add(-time.Hour)
	part := func(id, model, effort string, ts time.Time) schema.Event {
		e := ev(id, 500, schema.CostBilled)
		e.SessionID, e.Model, e.Effort, e.TS = "tied", model, effort, ts
		return e
	}
	ingest(t, d,
		part("first", "claude-sonnet-5", "max", at),
		part("second", "claude-opus-5", "low", at.Add(time.Minute)))

	rows, err := d.TopSessions(context.Background(), Window{From: "2000-01-01"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "claude-opus-5" || rows[0].Effort != "low" {
		t.Fatalf("got %+v, want the session named by the model and effort it used last", rows)
	}
}
