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

// A purge deletes rows before the re-read, so what is gone from disk is lost:
// only a harness that never deletes a record may be purged. The entries up to
// version 9 predate the finding that Codex and opencode delete records, and
// no archive older than 9 exists to run them.
func TestEverySourcePurgedOnUpgradeKeepsItsHistory(t *testing.T) {
	for v, srcs := range purgeOnUpgrade {
		for _, s := range srcs {
			a, ok := Lookup(string(s))
			if !ok {
				t.Errorf("version %d purges %q, which is not a registered source", v, s)
				continue
			}
			if v > 9 && !KeepsHistory(a) {
				t.Errorf("version %d purges %s, whose history does not outlive our "+
					"archive; match old rows to new ones in dedupeOnUpgrade instead", v, s)
			}
		}
	}
	for s, deletes := range map[schema.Source]string{
		schema.SourceClaudeCode: "deletes its transcripts after thirty days",
		schema.SourceCowork:     "deletes its transcripts after thirty days",
		schema.SourceCodex:      "can permanently delete a session",
		schema.SourceOpenCode:   "deletes a session's messages with it",
	} {
		if a, _ := Lookup(string(s)); KeepsHistory(a) {
			t.Errorf("%s %s; it must not claim to keep its history", s, deletes)
		}
	}
}

// Version 10 corrected opencode's subagent flag and re-keyed Continue. The
// bump alone changes only conflict resolution: without the re-read, stored
// rows keep what they were read with, and without the dedupe every Continue
// record is stored twice.
func TestCrossingTenReReadsWhatItCorrected(t *testing.T) {
	got := SourcesNeedingBackfill(9)
	for _, s := range []schema.Source{schema.SourceOpenCode, schema.SourceContinue} {
		if !slices.Contains(got, s) {
			t.Errorf("SourcesNeedingBackfill(9) = %v, missing %s", got, s)
		}
	}
	if got := SourcesNeedingDedupe(9); !slices.Equal(got, []schema.Source{schema.SourceContinue}) {
		t.Errorf("SourcesNeedingDedupe(9) = %v, want continue: version 10 re-keys it", got)
	}
}

// Invariant 9: a bump alone changes only conflict resolution. A version that
// names nothing to re-read leaves every stored row on the old reading.
func TestEveryCollectorVersionReReadsSomething(t *testing.T) {
	for v := 2; v <= CollectorVersion; v++ {
		if len(backfillOnUpgrade[v]) == 0 {
			t.Errorf("collector %d re-reads nothing: history keeps the reading it corrected", v)
		}
	}
}

// Dedupe deletes an old row only once its replacement is stored, and only a
// re-read stores one: deduped without it, a re-key never reaches history.
func TestEverySourceDedupedOnUpgradeIsReRead(t *testing.T) {
	for v, srcs := range dedupeOnUpgrade {
		for _, s := range srcs {
			if !slices.Contains(backfillOnUpgrade[v], s) {
				t.Errorf("collector %d dedupes %s without re-reading it", v, s)
			}
		}
	}
}

// Version 9 corrected four parsers (see CollectorVersion); a source dropped
// from its re-read keeps its pre-9 history as it was read.
func TestCrossingNineReReadsEverySourceItCorrected(t *testing.T) {
	got := SourcesNeedingBackfill(8)
	for _, s := range []schema.Source{schema.SourceCodex, schema.SourceContinue,
		schema.SourceClaudeCode, schema.SourceCowork} {
		if !slices.Contains(got, s) {
			t.Errorf("SourcesNeedingBackfill(8) = %v, missing %s", got, s)
		}
	}
}
