package sources

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// Adapter reads one harness. Adapters register themselves from init, so a
// harness is one file, with no central list to keep in sync.
type Adapter interface {
	Name() schema.Source
	// Roots are the home-relative directories this adapter owns. They scope
	// rewind and resync, and keep the unsupported-harness scanner from
	// reporting a directory we already read.
	Roots() []string
	// Collect reads whatever is new since the last pass.
	Collect(ctx context.Context, c *Ctx) (Result, error)
}

var registry = map[schema.Source]Adapter{}

// Register adds an adapter. Called from init; panics on a duplicate name,
// which can only be a programming error.
func Register(a Adapter) {
	if _, dup := registry[a.Name()]; dup {
		panic("sources: duplicate adapter " + string(a.Name()))
	}
	registry[a.Name()] = a
}

// All returns every registered adapter, ordered by name so output is stable.
func All() []Adapter {
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Lookup finds one adapter by name.
func Lookup(name string) (Adapter, bool) {
	a, ok := registry[schema.Source(name)]
	return a, ok
}

// Names lists registered adapter names.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, a := range All() {
		out = append(out, string(a.Name()))
	}
	return out
}

// Available reports whether any of an adapter's roots exist on this machine.
func Available(a Adapter, c *Ctx) bool {
	for _, r := range a.Roots() {
		if _, err := os.Stat(filepath.Join(c.Home, r)); err == nil {
			return true
		}
	}
	return false
}

// AbsRoots resolves an adapter's roots against the home directory, keeping
// only those that exist.
func AbsRoots(a Adapter, c *Ctx) []string {
	var out []string
	for _, r := range a.Roots() {
		p := filepath.Join(c.Home, r)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// metaOwner is implemented by an adapter that keeps state under a meta key
// not prefixed with its own name. New adapters should not need it: key
// everything "<source>:" and the default covers it.
type metaOwner interface {
	MetaPrefixes() []string
}

// historyKeeper is implemented by an adapter whose harness never drops a
// record it wrote, so stored rows can be rebuilt by re-reading.
type historyKeeper interface {
	KeepsHistory() bool
}

// KeepsHistory reports whether a source's own records outlive our archive.
// Only such a source may be purged on upgrade: for any other, deleting rows
// before the re-read loses whatever is no longer on disk. False unless the
// adapter says otherwise, because assuming it wrongly is permanent.
func KeepsHistory(a Adapter) bool {
	h, ok := a.(historyKeeper)
	return ok && h.KeepsHistory()
}

// ScopeOf is what `rewind` and `resync` clear for an adapter: every cursor
// under one of its own roots, and every meta key it writes.
//
// Derived from Roots(), the directories the walk actually reads, so the
// cursors it writes are exactly filepath.Join(home, root, ...). Anchored at
// home, so a Claude Code project named after another harness, or a home
// directory whose name contains one, is not swept in with it.
func ScopeOf(a Adapter, home string) store.Scope {
	sc := store.Scope{MetaPrefixes: []string{string(a.Name()) + ":"}}
	for _, r := range a.Roots() {
		sc.CursorPrefixes = append(sc.CursorPrefixes,
			filepath.Join(home, r)+string(filepath.Separator))
	}
	if m, ok := a.(metaOwner); ok {
		sc.MetaPrefixes = append(sc.MetaPrefixes, m.MetaPrefixes()...)
	}
	return sc
}
