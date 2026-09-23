package sources

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// covers reports whether a scope claims a cursor path, by the same literal
// prefix rule the store's DELETE applies.
func covers(prefixes []string, path string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

const home = "/Users/dev"

// cursorUnder is a cursor path as the walk really writes one.
func cursorUnder(root string) string {
	return filepath.Join(home, root, "s", "session.jsonl")
}

type ownedCursor struct {
	owner schema.Source
	path  string
}

// realCursors are cursor paths shaped like the ones on a real machine: every
// adapter's roots, Cowork's sessions -- which nest a whole .claude/projects
// tree, subagents included, under Application Support -- and Claude Code
// projects, whose directory names spell out the working directory and so can
// contain another harness's name or path.
func realCursors() []ownedCursor {
	var out []ownedCursor
	for _, a := range All() {
		for _, root := range a.Roots() {
			out = append(out, ownedCursor{a.Name(), cursorUnder(root)})
		}
	}
	const acct, org = "f9f0fbb1-e9cd-4ccf-b6cb-76676af45c9a", "110a9084-010d-4fd9-9406-e44138ad724f"
	for _, root := range (Cowork{}).Roots() {
		session := filepath.Join(home, root, acct, org, "local_7d2a053c",
			".claude/projects/-sessions-local-7d2a053c", "46a10fa7-dcef-44a3-897d-89e7a2abdc6c")
		out = append(out,
			ownedCursor{schema.SourceCowork, session + ".jsonl"},
			ownedCursor{schema.SourceCowork, filepath.Join(session, "subagents/agent-a08f065216d0451a2.jsonl")})
	}
	projects := []string{
		"-Users-dev-Library-Application-Support-Claude-local-agent-mode-sessions",
		"-Users-dev--codex-sessions",
		"-Users-dev--continue-dev-data",
	}
	for _, name := range Names() {
		projects = append(projects, "-Users-dev-src-"+name+"-bench")
	}
	for _, project := range projects {
		dir := filepath.Join(home, ".claude/projects", project)
		out = append(out,
			ownedCursor{schema.SourceClaudeCode, filepath.Join(dir, "s.jsonl")},
			ownedCursor{schema.SourceClaudeCode, filepath.Join(dir, "s/subagents/agent-a1.jsonl")})
	}
	return out
}

// A reset must reach every cursor the adapter's own walk wrote, or resync
// deletes its rows and leaves its cursors at EOF, so they are never rebuilt.
func TestEveryScopeCoversItsOwnCursors(t *testing.T) {
	for _, c := range realCursors() {
		a, _ := Lookup(string(c.owner))
		if !covers(ScopeOf(a, home).CursorPrefixes, c.path) {
			t.Errorf("%s would not rewind its own cursor %q", c.owner, c.path)
		}
	}
}

// No reset may reach another adapter's cursors, even through a path that
// merely contains a source's name or root: rewinding one source would re-read
// another.
func TestNoScopeReachesAnotherSourcesCursors(t *testing.T) {
	for _, a := range All() {
		sc := ScopeOf(a, home)
		for _, c := range realCursors() {
			if c.owner == a.Name() {
				continue
			}
			if covers(sc.CursorPrefixes, c.path) {
				t.Errorf("rewinding %s would also rewind %s's cursor %q",
					a.Name(), c.owner, c.path)
			}
		}
	}
}

// Every meta key an adapter writes must fall inside its scope, or rewind
// leaves a watermark in place and the re-read skips what it was meant to redo.
func TestScopeCoversTheMetaKeysAdaptersWrite(t *testing.T) {
	for src, key := range map[schema.Source]string{
		schema.SourceCodex:    "codexctx:/Users/dev/.codex/sessions/r.jsonl",
		schema.SourceOpenCode: "opencode:message_watermark",
		schema.SourceCline:    "cline:task:1",
		schema.SourceRooCode:  "roo_code:task:1",
	} {
		a, ok := Lookup(string(src))
		if !ok {
			t.Fatalf("%s is not registered", src)
		}
		if !covers(ScopeOf(a, home).MetaPrefixes, key) {
			t.Errorf("%s's scope misses its own key %q", src, key)
		}
	}
	// And not another's: "cline:" must not be a prefix of anything Roo Code
	// writes, nor the reverse.
	cline, _ := Lookup(string(schema.SourceCline))
	if covers(ScopeOf(cline, home).MetaPrefixes, "roo_code:task:1") {
		t.Error("cline's scope reaches roo_code's watermarks")
	}
}

// A version that re-keys one member of the Cline family re-keys both: they are
// one implementation registered twice.
func TestClineAndRooCodeAreUpgradedTogether(t *testing.T) {
	for v := 1; v <= CollectorVersion; v++ {
		for _, list := range []struct {
			what string
			got  []schema.Source
		}{
			{"backfill", SourcesNeedingBackfill(v - 1)},
			{"purge", SourcesNeedingPurge(v - 1)},
			{"dedupe", SourcesNeedingDedupe(v - 1)},
		} {
			hasCline := slices.Contains(list.got, schema.SourceCline)
			hasRoo := slices.Contains(list.got, schema.SourceRooCode)
			if hasCline != hasRoo {
				t.Errorf("crossing to version %d: %s names cline=%v roo_code=%v -- "+
					"they are one adapter registered twice, so a version that "+
					"invalidates one invalidates both", v, list.what, hasCline, hasRoo)
			}
		}
	}
}
