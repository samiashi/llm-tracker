package sources

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// candidate is a directory some coding harness is known to use.
type candidate struct {
	rel  string // relative to $HOME
	hint string
	// status defaults to todo; blocked entries carry the finding in note.
	status schema.UnknownStatus
	note   string
}

// candidates lists harnesses we know exist but have no adapter for, so a
// dashboard that omits them says so rather than looking complete.
//
// A todo entry is deleted once an adapter covers it, and a test enforces that
// so a harness is never reported as both supported and missing. A blocked entry
// stays; see ScanUnknown.
var candidates = []candidate{
	{rel: ".amp", hint: "Amp"},
	{rel: ".droid", hint: "Factory Droid"},
	{rel: ".goose", hint: "Goose"},
	{rel: ".qwen", hint: "Qwen Code"},
	{rel: ".cline", hint: "Cline"},
	{rel: ".grok", hint: "Grok CLI"},
	{rel: ".aider", hint: "Aider"},
	{rel: ".aider.tags.cache.v3", hint: "Aider"},
	{rel: ".factory", hint: "Factory"},
	// Blocked, not todo: these are the CLI halves of the two products below,
	// and the finding recorded there covers them. As todo they would report
	// one product as both "no adapter yet" and "cannot be done".
	{
		rel:    ".cursor",
		hint:   "Cursor CLI",
		status: schema.StatusBlocked,
		note: "The Cursor CLI's store.db carries no billed-usage ledger, the same " +
			"finding recorded for the Cursor app below. Cursor's own servers are the " +
			"only authority.",
	},
	{
		rel:    ".windsurf",
		hint:   "Windsurf CLI",
		status: schema.StatusBlocked,
		note: "Windsurf bills in prompt credits accounted server-side, the same " +
			"finding recorded for the Windsurf app below. No local request ledger " +
			"exists to read.",
	},

	{
		rel:    ".gemini/antigravity",
		hint:   "Antigravity",
		status: schema.StatusBlocked,
		note: "Conversations are protobuf blobs in per-session SQLite files with no " +
			"schema for them; none of its JSON files carry token fields. Nothing to read " +
			"without the .proto definitions.",
	},
	{
		rel:    "Library/Application Support/Code/User/globalStorage/github.copilot-chat",
		hint:   "Copilot Chat (VS Code)",
		status: schema.StatusBlocked,
		note: "Its session store has no token columns at all -- only sessions, turns and " +
			"a search index. The bulk of the directory is an embeddings cache, not usage.",
	},

	// Listed individually rather than as the whole globalStorage directory:
	// that folder holds every extension's state, so reporting it entire says
	// "something in here uses tokens", which is true and useless.
	{rel: "Library/Application Support/Code/User/globalStorage/continue.continue", hint: "Continue (VS Code)"},

	{
		rel:    "Library/Application Support/Cursor",
		hint:   "Cursor app",
		status: schema.StatusBlocked,
		note: "No billed-usage ledger exists on disk. state.vscdb still attaches " +
			"tokenCount to message bubbles, but those counters are unused; composer " +
			"usageData is empty; and promptTokenBreakdown / contextTokensUsed measure " +
			"context-window occupancy, not what was charged for the request. The Cursor " +
			"CLI's store.db carries no substitute. Reading any of them would produce " +
			"confident, wrong numbers. Cursor's own servers are the only authority: " +
			"Enterprise plans can push tokens and cost to a collector over OpenTelemetry, " +
			"which is a server-side integration rather than an adapter.",
	},
	{
		rel:    "Library/Application Support/Windsurf",
		hint:   "Windsurf app",
		status: schema.StatusBlocked,
		note: "Same shape as Cursor: a VS Code fork whose credit accounting lives " +
			"server-side. The local state.vscdb holds conversation state, not a request " +
			"ledger, and Windsurf bills in prompt credits rather than tokens -- so even " +
			"an accurate local count would not convert into the units this dashboard adds up.",
	},

	// Cline inside Windsurf. The adapters read the Code and Cursor copies;
	// Windsurf keeps its own globalStorage, which neither of them looks in.
	{rel: "Library/Application Support/Windsurf/User/globalStorage/saoudrizwan.claude-dev", hint: "Cline (Windsurf)"},

	// Deliberately absent: the ChatGPT desktop app. Its conversation store is
	// opaque binary with no usage fields anywhere, so it is not even a blocked
	// candidate -- it belongs in the README's not-coverable list.
}

// coveredRoots is the set of home-relative paths some registered adapter owns.
func coveredRoots() []string {
	var out []string
	for _, a := range All() {
		out = append(out, a.Roots()...)
	}
	return out
}

// isCovered reports whether rel is, contains or lies inside a root some adapter
// reads -- so an adapter on ".codex/sessions" also suppresses a candidate entry
// for ".codex".
func isCovered(rel string, covered []string) bool {
	rel = strings.TrimSuffix(rel, "/")
	for _, c := range covered {
		c = strings.TrimSuffix(c, "/")
		if rel == c || strings.HasPrefix(c, rel+"/") || strings.HasPrefix(rel, c+"/") {
			return true
		}
	}
	return false
}

// ScanUnknown records harness directories present on this machine that no
// registered adapter covers.
func ScanUnknown(ctx context.Context, c *Ctx) (int, error) {
	covered := coveredRoots()
	found := 0
	for _, cand := range candidates {
		// Suppression answers "do we already read this?", which is not the
		// question a blocked entry answers. Cline's tasks live under the Cursor
		// application directory when it is installed inside Cursor, and reading
		// those would otherwise hide the finding that Cursor's *own* usage
		// cannot be read at all -- the more useful of the two facts.
		if cand.status != schema.StatusBlocked && isCovered(cand.rel, covered) {
			continue
		}
		p := filepath.Join(c.Home, cand.rel)
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		size, truncated := approxSize(p)
		if size == 0 {
			continue
		}
		hint := cand.hint
		if truncated {
			hint += " (size is a lower bound)"
		}
		status := cand.status
		if status == "" {
			status = schema.StatusTodo
		}
		if err := c.Store.PutUnknownSource(ctx, p, hint, size, string(status), cand.note); err != nil {
			return found, err
		}
		found++
	}
	return found, nil
}

// approxSize sums file sizes under root, bounded by a file count so that a
// stray enormous directory cannot stall a collection pass. Depth is not
// capped: harnesses nest deeply, and an understated size misjudges whether one
// is worth an adapter.
func approxSize(root string) (total int64, truncated bool) {
	const maxFiles = 50_000
	var seen int
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if seen >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		if !d.IsDir() {
			seen++
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, truncated
}
