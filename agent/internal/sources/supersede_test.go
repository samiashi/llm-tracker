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
		if e.NativeID != "0.1.0/tokensGenerated.jsonl#0" && e.NativeID != "0.2.0/tokensGenerated.jsonl#0" {
			t.Errorf("native id %q: want the path under dev_data and the offset", e.NativeID)
		}
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
			event(schema.SourceContinue, "/Users/dev/.continue/dev_data/0.2.0/tokensGenerated.jsonl#20260920T100000.000#m", "", at, "m", 10),
			event(schema.SourceContinue, "tokensGenerated.jsonl#0", "", at, "m", 10),  // replaced by 0.2.0's
			event(schema.SourceContinue, "tokensGenerated.jsonl#64", "", at, "m", 30), // no replacement
			event(schema.SourceContinue, "0.2.0/tokensGenerated.jsonl#0", "", at, "m", 10),
			event(schema.SourceContinue, "0.1.0/tokensGenerated.jsonl#0", "", later, "m", 20),
			event(schema.SourceContinue, "/Users/dev/.continue/dev_data/x/tokensGenerated.jsonl#20260920T100001.000#m", "", later, "m", 99),
		}
		got := Superseded(schema.SourceContinue, evs)
		want := []string{evs[0].ID, evs[1].ID}
		if !slices.Equal(got, want) {
			t.Fatalf("Superseded = %v, want %v", got, want)
		}
	})
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
