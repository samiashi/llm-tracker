package sources

import "github.com/samiashi/llm-tracker/schema"

// CollectorVersion is the version of the collection code.
//
// Bump it whenever an adapter starts capturing something or corrects what it
// captured, and list the sources that invalidates in backfillOnUpgrade. A
// newer collector's reading replaces an older one's unless it counts fewer
// tokens, which is how a backfill reaches rows already stored.
//
//	2: Codex reads usage from token_count, not only token_usage_record.
//	3: opencode maps its model "variant" onto reasoning effort.
//	4: Cline, Roo Code and Continue are read.
//	5: Codex, Cline, Roo Code and Continue key events on something that does
//	   not move: a basename, a timestamp, a byte offset.
//	6: Quota samples name the allowance pool they measure.
//	7: opencode is read per response rather than per session.
//	8: opencode folds reasoning tokens into output.
//	9: Codex keys token_count usage on its content, and labels, dates and
//	   attributes each response from the rollout's own header and time;
//	   Continue keys on the file's path under dev_data; Claude Code and
//	   Cowork count the attempt a model fallback abandoned.
//	10: opencode marks a subagent by its child session, not its agent's name.
const CollectorVersion = 10

// backfillOnUpgrade names, for each collector version, the sources whose
// stored rows that version invalidates. Crossing it resets their cursors, so
// the next pass re-reads them; the bump alone changes only conflict
// resolution, and history read before it would keep the old reading.
var backfillOnUpgrade = map[int][]schema.Source{
	2: {schema.SourceCodex},
	3: {schema.SourceOpenCode},
	4: {schema.SourceCline, schema.SourceRooCode, schema.SourceContinue},
	5: {
		schema.SourceCodex,
		schema.SourceCline,
		schema.SourceRooCode,
		schema.SourceContinue,
	},
	6: {schema.SourceCodex, schema.SourceCowork},
	7: {schema.SourceOpenCode},
	8: {schema.SourceOpenCode},
	9: {
		schema.SourceCodex,      // re-keyed; subagent flag, session, account and endpoint corrected
		schema.SourceContinue,   // re-keyed on the path under dev_data
		schema.SourceClaudeCode, // a fallback's abandoned attempt is newly counted
		schema.SourceCowork,     // the same parser as Claude Code
	},
	10: {
		schema.SourceOpenCode, // subagent flag corrected
	},
}

// purgeOnUpgrade names the sources a version re-keys where no old row can be
// matched to its replacement, so their rows are deleted before the re-read: a
// backfill reads over old rows without removing them, and rows nothing will
// touch again double every token they hold.
//
// Destructive, so only for a source that keeps its own history (KeepsHistory,
// enforced by a test). Where a match is possible, dedupeOnUpgrade is used
// instead. The server removes the same rows in its migrations.
//
// The entries below predate the finding that Codex and opencode delete
// records. They run only for an archive older than collector 9, and there is
// none: every archive starts at 9 or later.
var purgeOnUpgrade = map[int][]schema.Source{
	5: {schema.SourceCodex},    // off the absolute path
	7: {schema.SourceOpenCode}, // one event per response, not per session
	9: {schema.SourceCodex},    // token_count usage keyed on content, not position
}

// dedupeOnUpgrade names the sources a version re-keys where each old row can
// be matched to the row that replaces it. Once the re-read has written the
// replacements, Superseded names the old rows and the collector deletes them.
// Unlike a purge this keeps a row whose record is gone from disk, which is why
// harnesses that let the user delete their history are listed here.
var dedupeOnUpgrade = map[int][]schema.Source{
	5: {schema.SourceCline, schema.SourceRooCode, schema.SourceContinue},
	9: {schema.SourceContinue},
}

// SourcesNeedingPurge returns the sources whose rows must be discarded, rather
// than re-read over, when moving to the current collector version.
func SourcesNeedingPurge(stored int) []schema.Source {
	return sourcesSince(stored, purgeOnUpgrade)
}

// SourcesNeedingBackfill returns the sources to re-read when moving from a
// stored collector version to the current one.
func SourcesNeedingBackfill(stored int) []schema.Source {
	return sourcesSince(stored, backfillOnUpgrade)
}

// SourcesNeedingDedupe returns the sources whose superseded rows must be
// removed, after the re-read that writes their replacements, when moving from
// a stored collector version to the current one.
func SourcesNeedingDedupe(stored int) []schema.Source {
	return sourcesSince(stored, dedupeOnUpgrade)
}

// sourcesSince collects, without repeats, the sources a table names for every
// version after stored up to the current one.
func sourcesSince(stored int, byVersion map[int][]schema.Source) []schema.Source {
	if stored >= CollectorVersion {
		return nil
	}
	seen := map[schema.Source]bool{}
	var out []schema.Source
	for v := stored + 1; v <= CollectorVersion; v++ {
		for _, src := range byVersion[v] {
			if !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	return out
}
