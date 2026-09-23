package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// The canary counts the live events of each source on each machine and finds
// the last of them: a rolled-up day has no event times, and counted here it
// would pad a source that has since gone quiet.
func TestSourceHealthCountsLiveEventsAndFindsTheLast(t *testing.T) {
	d := newDB(t)
	last := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(e schema.Event, ts time.Time) schema.Event {
		e.TS = ts
		return e
	}
	codex := ev("codex", 10, schema.CostBilled)
	codex.Source = schema.SourceCodex
	ingest(t, d,
		at(ev("early", 10, schema.CostBilled), last.Add(-24*time.Hour)),
		at(ev("late", 10, schema.CostBilled), last),
		at(codex, last.Add(-2*time.Hour)),
		oldDay("pruned", 1, 10)[0])
	pruneOld(t, d)

	got, err := d.SourceHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []SourceHealth{
		{Source: "claude_code", MachineID: "m", LastEvent: last.Unix(), Events: 2},
		{Source: "codex", MachineID: "m", LastEvent: last.Add(-2 * time.Hour).Unix(), Events: 1},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("source health %+v, want %+v", got, want)
	}
}
