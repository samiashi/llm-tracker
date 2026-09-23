package db

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// Ranked by its $0, a session on a model the table does not know sits below
// every priced one and never shows: the silent zero invariant 8 forbids. Its
// unpriced tokens place it instead, so a large one shows and a trivial one
// does not jump the queue.
func TestALargeUnpricedSessionStaysInTheList(t *testing.T) {
	d := newDB(t)
	var evs []schema.Event
	for i := 1; i <= 12; i++ {
		e := ev(fmt.Sprintf("priced-%d", i), 1_000, schema.CostBilled)
		e.SessionID = fmt.Sprintf("priced-%02d", i)
		usd := float64(i)
		e.NativeCostUSD = &usd
		evs = append(evs, e)
	}
	unpriced := func(id string, tok int64) schema.Event {
		e := ev(id, tok, schema.CostBilled)
		e.SessionID, e.Model = id, "not-a-real-model"
		return e
	}
	evs = append(evs, unpriced("large", 1_000_000), unpriced("trivial", 10))
	ingest(t, d, evs...)

	rows, err := d.TopSessions(context.Background(), Window{From: "2000-01-01"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.SessionID)
		if want := map[string]int64{"large": 1_000_000}[r.SessionID]; r.UnpricedTokens != want {
			t.Errorf("%s: unpriced_tokens = %d, want %d", r.SessionID, r.UnpricedTokens, want)
		}
	}
	want := []string{"priced-12", "large", "priced-11", "priced-10", "priced-09", "priced-08",
		"priced-07", "priced-06", "priced-05", "priced-04"}
	if !slices.Equal(got, want) {
		t.Fatalf("sessions %v, want %v", got, want)
	}
}

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
