package sources

import (
	"slices"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

func TestSourcesNeedingBackfill(t *testing.T) {
	if got := SourcesNeedingBackfill(0); len(got) == 0 {
		t.Fatal("upgrading from nothing should name the affected sources")
	}
	if got := SourcesNeedingBackfill(CollectorVersion); got != nil {
		t.Errorf("an up-to-date collector wants a backfill of %v", got)
	}
	if got := SourcesNeedingBackfill(CollectorVersion + 1); got != nil {
		t.Errorf("a newer stored version wants a backfill of %v", got)
	}

	// Spanning several versions accumulates them without repeats; codex is
	// named by more than one.
	got := SourcesNeedingBackfill(1)
	seen := map[schema.Source]int{}
	for _, s := range got {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("%s listed %d times", s, n)
		}
	}
	if seen[schema.SourceCodex] != 1 {
		t.Errorf("codex should appear exactly once across versions 2..%d", CollectorVersion)
	}

	// Every named source must actually exist, or a rename silently stops the
	// backfill it was meant to trigger.
	for v, srcs := range backfillOnUpgrade {
		if v > CollectorVersion {
			t.Errorf("version %d is in the table but above CollectorVersion", v)
		}
		for _, s := range srcs {
			if _, ok := Lookup(string(s)); !ok {
				t.Errorf("version %d names %q, which is not a registered source", v, s)
			}
		}
	}
}

func TestAnIdentityChangePurgesWhatItReplaces(t *testing.T) {
	if got := SourcesNeedingPurge(6); !slices.Contains(got, schema.SourceOpenCode) {
		t.Fatalf("SourcesNeedingPurge(6) = %v, want opencode: version 7 reads it per response", got)
	}
	if got := SourcesNeedingPurge(8); !slices.Equal(got, []schema.Source{schema.SourceCodex}) {
		t.Fatalf("SourcesNeedingPurge(8) = %v, want codex: version 9 keys its usage on content", got)
	}
	if n := SourcesNeedingPurge(CollectorVersion); n != nil {
		t.Fatalf("a current collector purges %v; it must purge nothing", n)
	}
}

func TestEverySourcePurgedOnUpgradeKeepsItsHistory(t *testing.T) {
	for v, srcs := range purgeOnUpgrade {
		for _, s := range srcs {
			a, ok := Lookup(string(s))
			if !ok {
				t.Errorf("version %d purges %q, which is not a registered source", v, s)
				continue
			}
			if !KeepsHistory(a) {
				t.Errorf("version %d purges %s, whose history does not outlive our "+
					"archive; match old rows to new ones in dedupeOnUpgrade instead", v, s)
			}
		}
	}
	for _, s := range []schema.Source{schema.SourceClaudeCode, schema.SourceCowork} {
		if a, _ := Lookup(string(s)); KeepsHistory(a) {
			t.Errorf("%s deletes its transcripts after thirty days; it must not claim to keep them", s)
		}
	}
}
