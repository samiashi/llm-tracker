package sources

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

func TestContinueKeepsVersionDirectoriesApart(t *testing.T) {
	home := t.TempDir()
	for dir, line := range map[string]string{
		"0.1.0": `{"timestamp":"2026-09-20T10:00:00Z","model":"claude-opus-5","promptTokens":100,"generatedTokens":10}`,
		"0.2.0": `{"timestamp":"2026-09-21T10:00:00Z","model":"gpt-6","promptTokens":5000,"generatedTokens":500}`,
	} {
		p := filepath.Join(home, ".continue/dev_data", dir, "tokensGenerated.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := (ContinueDev{}).Collect(context.Background(), &Ctx{Store: st, MachineID: "m", Home: home}); err != nil {
		t.Fatal(err)
	}

	evs := storedEvents(t, st)
	if len(evs) != 2 || totalOf(evs) != 110+5500 {
		t.Fatalf("stored %d events / %d tokens, want 2 / %d", len(evs), totalOf(evs), 110+5500)
	}
	for _, e := range evs {
		if e.NativeID != "m:0.1.0/tokensGenerated.jsonl#0" && e.NativeID != "m:0.2.0/tokensGenerated.jsonl#0" {
			t.Errorf("native id %q: want the machine, the path under dev_data and the offset", e.NativeID)
		}
	}
}

// Every file starts at offset 0, so without the machine two colleagues'
// records share an id, and the server merges them into one.
func TestContinueRecordsFromTwoMachinesStayApart(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".continue/dev_data/0.2.0/tokensGenerated.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-09-20T10:00:00Z","model":"claude-opus-5","promptTokens":100,"generatedTokens":10}`
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, machine := range []string{"m1", "m2"} {
		st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if _, err := (ContinueDev{}).Collect(context.Background(), &Ctx{Store: st, MachineID: machine, Home: home}); err != nil {
			t.Fatal(err)
		}
		evs := storedEvents(t, st)
		if len(evs) != 1 {
			t.Fatalf("%s stored %d events, want 1", machine, len(evs))
		}
		ids[machine] = evs[0].ID
	}
	if ids["m1"] == ids["m2"] {
		t.Fatalf("both machines' records are %s: the server would merge them", ids["m1"])
	}
}

func event(src schema.Source, nid, session string, ts time.Time, model string, in int64) schema.Event {
	return schema.Event{
		ID: schema.MakeID(src, nid), NativeID: nid, Source: src, SessionID: session,
		TS: ts, Model: model, Usage: schema.Usage{InputTokens: in, OutputTokens: 1},
	}
}

func TestSupersededNamesOnlyOldRowsWhoseReplacementIsPresent(t *testing.T) {
	at := time.UnixMilli(1790000001000).UTC()
	later := at.Add(time.Second)

	t.Run("cline", func(t *testing.T) {
		evs := []schema.Event{
			event(schema.SourceCline, "task#4", "task", at, "m", 10),                 // replaced below
			event(schema.SourceCline, "task#7", "task", at, "m", 10),                 // same request, index shifted
			event(schema.SourceCline, "task#9", "task", later, "m", 20),              // its request is gone
			event(schema.SourceCline, "task#1790000001000#0", "task", at, "m", 10),   // current
			event(schema.SourceCline, "other#1790000001000#0", "other", at, "m", 20), // another task
		}
		got := Superseded(schema.SourceCline, evs)
		want := []string{evs[0].ID, evs[1].ID}
		if !slices.Equal(got, want) {
			t.Fatalf("Superseded = %v, want %v", got, want)
		}
	})

	t.Run("continue", func(t *testing.T) {
		evs := []schema.Event{
			continueRow("/Users/dev/.continue/dev_data/0.2.0/tokensGenerated.jsonl#20260920T100000.000#m", "m", 4, at, 10),
			continueRow("tokensGenerated.jsonl#0", "m", 8, at, 10),          // replaced by 0.2.0's
			continueRow("tokensGenerated.jsonl#64", "m", 8, at, 30),         // no replacement
			continueRow("0.2.0/tokensGenerated.jsonl#0", "m", 9, at, 10),    // replaced by the machine's
			continueRow("m:0.2.0/tokensGenerated.jsonl#0", "m", 10, at, 10), // current
			continueRow("0.1.0/tokensGenerated.jsonl#0", "m", 9, later, 20), // its record is gone
			continueRow("/Users/dev/.continue/dev_data/x/tokensGenerated.jsonl#20260920T100001.000#m", "m", 4, later, 99),
		}
		got := Superseded(schema.SourceContinue, evs)
		want := []string{evs[0].ID, evs[1].ID, evs[3].ID}
		if !slices.Equal(got, want) {
			t.Fatalf("Superseded = %v, want %v", got, want)
		}
	})
}

// continueRow is a Continue record as a collector keyed it on a machine.
func continueRow(nid, machine string, collector int, ts time.Time, in int64) schema.Event {
	e := event(schema.SourceContinue, nid, "", ts, "m", in)
	e.MachineID, e.Collector = machine, collector
	return e
}

// Superseded deletes from the only lasting copy, so an old row goes only for
// the one row that is the same record: agreeing on all but one part of its
// key, a row is a different record, and may be the only one left of it.
func TestSupersededPairsAnOldRowOnlyWithItsOwnReplacement(t *testing.T) {
	at := time.UnixMilli(1790000001000).UTC()
	later := at.Add(time.Second)
	withModel := func(e schema.Event, model string) schema.Event { e.Model = model; return e }
	withOutput := func(e schema.Event, out int64) schema.Event { e.Usage.OutputTokens = out; return e }
	const pathKey = "/Users/dev/.continue/dev_data/0.2.0/tokensGenerated.jsonl#20260920T100000.000#m"

	for _, tc := range []struct {
		name string
		src  schema.Source
		old  schema.Event
		by   schema.Event
		gone bool
	}{
		{"cline: its replacement", schema.SourceCline,
			event(schema.SourceCline, "task#4", "task", at, "m", 10),
			event(schema.SourceCline, "task#1790000001000#0", "task", at, "m", 10), true},
		{"cline: another request in the same millisecond", schema.SourceCline,
			event(schema.SourceCline, "task#2", "task", at, "m", 11),
			event(schema.SourceCline, "task#1790000001000#0", "task", at, "m", 10), false},
		{"cline: the same usage a second later", schema.SourceCline,
			event(schema.SourceCline, "task#5", "task", later, "m", 10),
			event(schema.SourceCline, "task#1790000001000#0", "task", at, "m", 10), false},
		{"cline: another task", schema.SourceCline,
			event(schema.SourceCline, "gone#1", "gone", at, "m", 10),
			event(schema.SourceCline, "task#1790000001000#0", "task", at, "m", 10), false},

		{"continue 9: its replacement on the same machine", schema.SourceContinue,
			continueRow("0.2.0/tokensGenerated.jsonl#40", "m", 9, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#40", "m", 10, at, 10), true},
		{"continue 9: a root-level file's replacement", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 9, at, 10),
			continueRow("m:tokensGenerated.jsonl#40", "m", 10, at, 10), true},
		{"continue 9: another machine's record at the same place", schema.SourceContinue,
			continueRow("0.2.0/tokensGenerated.jsonl#40", "m", 9, at, 10),
			continueRow("n:0.2.0/tokensGenerated.jsonl#40", "n", 10, at, 10), false},
		{"continue 9: the same machine's record at another offset", schema.SourceContinue,
			continueRow("0.2.0/tokensGenerated.jsonl#40", "m", 9, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#41", "m", 10, at, 10), false},
		{"continue 9: a root-level file's row beside a versioned twin", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#0", "m", 9, at, 10),
			continueRow("0.2.0/tokensGenerated.jsonl#0", "m", 9, later, 10), false},
		{"continue 9: a root-level file's row beside a re-keyed versioned twin", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#0", "m", 9, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#0", "m", 10, later, 10), false},

		{"continue 5-8: replaced under a directory", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("0.2.0/tokensGenerated.jsonl#40", "m", 9, later, 10), true},
		{"continue 5-8: replaced under the machine", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("m:tokensGenerated.jsonl#40", "m", 10, later, 10), true},
		{"continue 5-8: replaced under the machine and a directory", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#40", "m", 10, later, 10), true},
		{"continue 5-8: another model", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			withModel(continueRow("m:0.2.0/tokensGenerated.jsonl#40", "m", 10, at, 10), "other"), false},
		{"continue 5-8: other input", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#40", "m", 10, at, 11), false},
		{"continue 5-8: other output", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			withOutput(continueRow("m:0.2.0/tokensGenerated.jsonl#40", "m", 10, at, 10), 2), false},
		{"continue 5-8: another machine", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("n:0.2.0/tokensGenerated.jsonl#40", "n", 10, at, 10), false},
		{"continue 5-8: another offset", schema.SourceContinue,
			continueRow("tokensGenerated.jsonl#40", "m", 8, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#140", "m", 10, at, 10), false},

		{"continue before 5: replaced at the same time", schema.SourceContinue,
			continueRow(pathKey, "m", 4, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#0", "m", 10, at, 10), true},
		{"continue before 5: a record at another time", schema.SourceContinue,
			continueRow(pathKey, "m", 4, at, 10),
			continueRow("m:0.2.0/tokensGenerated.jsonl#0", "m", 10, later, 10), false},
		{"continue before 5: another machine", schema.SourceContinue,
			continueRow(pathKey, "m", 4, at, 10),
			continueRow("n:0.2.0/tokensGenerated.jsonl#0", "n", 10, at, 10), false},
		{"continue before 5: another model", schema.SourceContinue,
			continueRow(pathKey, "m", 4, at, 10),
			withModel(continueRow("m:0.2.0/tokensGenerated.jsonl#0", "m", 10, at, 10), "other"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Superseded(tc.src, []schema.Event{tc.old, tc.by})
			if gone := slices.Contains(got, tc.old.ID); gone != tc.gone {
				t.Fatalf("Superseded = %v: old row deleted = %v, want %v", got, gone, tc.gone)
			}
			if slices.Contains(got, tc.by.ID) {
				t.Fatalf("Superseded = %v: deleted the current row", got)
			}
		})
	}
}

func TestRekeyedHarnessesThatDropHistoryAreDedupedNotPurged(t *testing.T) {
	for _, s := range SourcesNeedingDedupe(0) {
		a, ok := Lookup(string(s))
		if !ok {
			t.Fatalf("dedupeOnUpgrade names %q, which is not registered", s)
		}
		if KeepsHistory(a) {
			continue
		}
		if slices.Contains(SourcesNeedingPurge(0), s) {
			t.Errorf("%s is purged on upgrade although its history can disappear from disk", s)
		}
	}
	for _, s := range []schema.Source{schema.SourceCline, schema.SourceRooCode, schema.SourceContinue} {
		if !slices.Contains(SourcesNeedingDedupe(4), s) {
			t.Errorf("crossing version 5 must dedupe %s's re-keyed rows", s)
		}
	}
	if got := SourcesNeedingDedupe(8); !slices.Equal(got, []schema.Source{schema.SourceContinue}) {
		t.Errorf("SourcesNeedingDedupe(8) = %v, want continue: version 9 re-keys it", got)
	}
}
