// Package sources holds one adapter per harness.
//
// Every adapter is deliberately shallow: find the records that carry token
// counts, map them onto schema.Event, and ignore everything else. None of them
// read message text. The formats are private, undocumented and change without
// notice, so adapters are expected to break, and the collector's job is to
// notice loudly rather than quietly report less.
package sources

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// Ctx carries what an adapter needs for one collection pass and collects what
// it emits, so no adapter threads its own slices through nested callbacks.
type Ctx struct {
	Store     *store.Store
	MachineID string
	Home      string
	// Accounts is keyed by provider ("anthropic", "openai") and holds the
	// account active right now: AccountRefAt's fallback for a provider with
	// no recorded switch.
	Accounts map[string]*identity.Account
	// AccountHistory is every observed switch, oldest first, so an event can
	// be attributed by its own timestamp.
	AccountHistory []store.AccountWindow

	events   []schema.Event
	quota    []schema.QuotaSample
	quotaIdx map[string]int
	// unparsed counts lines an adapter read and could not decode, folded
	// into Result at the end of a walk.
	unparsed int
	// meta is per-file parser state staged to commit with the current file's
	// rows and cursor, rather than after the walk. See store.CommitFile.
	meta map[string]string

	// Totals counts everything emitted across the whole pass, since the
	// per-unit buffers are drained as they are committed.
	Totals struct{ Events, Quota int }
}

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
const CollectorVersion = 9

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
}

// purgeOnUpgrade names the sources a version re-keys where no old row can be
// matched to its replacement, so their rows are deleted before the re-read: a
// backfill reads over old rows without removing them, and rows nothing will
// touch again double every token they hold.
//
// Destructive, so only for a source that keeps its own history (KeepsHistory,
// enforced by a test). Where a match is possible, dedupeOnUpgrade is used
// instead. The server removes the same rows in its migrations.
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

// maxWholeFileBytes caps an adapter that must read a file entire rather than
// tailing it. Generous enough for any real session aggregate, small enough
// that a runaway file cannot be pulled into a background daemon's memory.
const maxWholeFileBytes = 64 << 20

// Unparsed records a line the adapter could not decode.
func (c *Ctx) Unparsed() { c.unparsed++ }

// takeUnparsed returns the count and resets it, so each adapter's Result
// reports only its own failures.
func (c *Ctx) takeUnparsed() int {
	n := c.unparsed
	c.unparsed = 0
	return n
}

// AccountRef returns the currently active account for a provider, or "".
func (c *Ctx) AccountRef(provider string) string {
	if a := c.Accounts[provider]; a != nil {
		return a.Ref
	}
	return ""
}

// AccountRefAt returns the account that was active at a given moment.
//
// Harnesses that write no account into their logs must be attributed this way,
// or reading a month-old session credits it to whoever is signed in today.
// Events older than the first recorded switch fall back to the earliest known
// account: a guess, since nothing on disk records who produced them.
func (c *Ctx) AccountRefAt(provider string, ts time.Time) string {
	best := ""
	for _, w := range c.AccountHistory {
		if w.Provider != provider {
			continue
		}
		if best == "" || !w.ObservedAt.After(ts) {
			best = w.Ref
		}
		if w.ObservedAt.After(ts) {
			break
		}
	}
	if best == "" {
		return c.AccountRef(provider)
	}
	return best
}

// emit records one usage event.
func (c *Ctx) emit(e schema.Event) { c.events = append(c.events, e) }

// emitQuota records a quota reading, collapsing repeats within the same bucket
// to their highest value. Codex attaches rate limits to every response, so
// collapsing at capture keeps a million readings of one gauge out of memory.
func (c *Ctx) emitQuota(q schema.QuotaSample) {
	if c.quotaIdx == nil {
		c.quotaIdx = make(map[string]int)
	}
	if i, ok := c.quotaIdx[q.ID]; ok {
		if q.UsedPercent > c.quota[i].UsedPercent {
			c.quota[i] = q
		}
		return
	}
	c.quotaIdx[q.ID] = len(c.quota)
	c.quota = append(c.quota, q)
}

// Drain takes everything emitted since the last call, so each adapter's output
// can be attributed to it.
func (c *Ctx) Drain() ([]schema.Event, []schema.QuotaSample) {
	e, q := c.events, c.quota
	c.events, c.quota, c.quotaIdx = nil, nil, nil
	return e, q
}

// discard drops what a failed unit emitted, so nothing half-parsed is stored.
func (c *Ctx) discard() {
	c.events, c.quota, c.quotaIdx, c.meta = nil, nil, nil, nil
}

// StageMeta records parser state to be written in the same transaction as the
// current file's rows and cursor. Last write for a key wins.
func (c *Ctx) StageMeta(key, value string) {
	if c.meta == nil {
		c.meta = map[string]string{}
	}
	c.meta[key] = value
}

// drainMeta takes the staged meta, leaving the buffer empty.
func (c *Ctx) drainMeta() map[string]string {
	m := c.meta
	c.meta = nil
	return m
}

// commitFile persists what a file produced together with its read position.
func (c *Ctx) commitFile(ctx context.Context, path string, consumed, offset, size int64) (int, error) {
	events, quota := c.Drain()
	c.Totals.Events += len(events)
	c.Totals.Quota += len(quota)
	meta := c.drainMeta()
	// Nothing read and nothing to store, so no cursor to move: rewriting an
	// identical one costs a write transaction per unchanged file per pass.
	if len(events) == 0 && len(quota) == 0 && len(meta) == 0 && consumed == 0 {
		return 0, nil
	}
	return c.Store.CommitFile(ctx, path, offset, size,
		toRecords(events), toQuotaRecords(quota), meta)
}

// CommitPending persists anything emitted outside a file walk, for adapters
// that read a database rather than a log. It is commitFile with no file: an
// empty path moves no cursor.
func (c *Ctx) CommitPending(ctx context.Context) (int, error) {
	return c.commitFile(ctx, "", 0, 0, 0)
}

func toRecords(events []schema.Event) []store.Record {
	out := make([]store.Record, 0, len(events))
	for i := range events {
		e := events[i]
		e.Collector = CollectorVersion
		out = append(out, store.Record{
			ID: e.ID, TS: e.TS, TotalTokens: e.Usage.TotalTokens(),
			Collector: CollectorVersion, Payload: e,
		})
	}
	return out
}

func toQuotaRecords(quota []schema.QuotaSample) []store.Record {
	out := make([]store.Record, 0, len(quota))
	for i := range quota {
		q := quota[i]
		// Utilisation is scaled into the same discriminator the conflict rule
		// uses, so the highest reading in an hourly bucket is the one kept.
		out = append(out, store.Record{
			ID: q.ID, TS: q.TS, TotalTokens: int64(q.UsedPercent * 1000),
			Collector: CollectorVersion, Payload: q,
		})
	}
	return out
}

// Result is what one adapter did in one pass. The events themselves live on
// the Ctx; this is the bookkeeping.
type Result struct {
	Files     int
	BytesRead int64
	// Scanned counts records examined that are not bytes of a file, so the
	// silent-source alarm ("read something, understood nothing") also covers
	// a database-backed adapter, which never sets BytesRead.
	Scanned int
	// Stored counts rows the pass wrote: new ones, and old ones a better
	// reading upgraded.
	Stored int
	// Errors are per-file problems that did not stop the pass.
	Errors []error
	// Unparsed counts lines an adapter read and could not decode, so a
	// harness renaming its fields does not look like a quiet afternoon.
	Unparsed int
}

// tailJSONL reads only the bytes appended to path since the last pass and
// hands each complete line to fn.
//
// Three details matter. A line that is still being written is left alone, so
// the cursor never lands mid-record and the partial line is picked up whole on
// the next pass. A file that has shrunk was rotated or replaced, so reading
// restarts from zero. And lines are read with ReadBytes rather than a Scanner
// because Claude Code records routinely exceed bufio.Scanner's limit, which
// would otherwise abort the file with a confusing error.
func tailJSONL(ctx context.Context, st *store.Store, path string, fn func(at int64, line []byte)) (consumedBytes, newOffset, fileSize int64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	size := fi.Size()

	offset, lastSize, err := st.Cursor(ctx, path)
	if err != nil {
		return 0, 0, 0, err
	}
	if size < offset || size < lastSize {
		offset = 0
	}
	if offset >= size {
		return 0, offset, size, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, 0, 0, err
	}

	r := bufio.NewReaderSize(f, 256*1024)
	var consumed int64
	for ctx.Err() == nil {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// An unterminated trailing line is an in-flight write.
			break
		}
		// The line's own start: the only identifier some formats have, and
		// the cursor guarantees each offset is read exactly once.
		at := offset + consumed
		consumed += int64(len(line))
		if t := bytes.TrimSpace(line); len(t) > 0 {
			fn(at, t)
		}
	}

	// Not persisted here: the caller commits the cursor with the rows it
	// covers, or an interrupted pass marks bytes read whose events were never
	// stored.
	return consumed, offset + consumed, size, nil
}

// parseTS accepts the RFC3339 timestamps every harness here emits.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// fileMTime is the fallback timestamp for records a harness did not date.
// The zero time would park them before every window the dashboard asks for,
// which reads as "no usage" rather than "usage we could not date".
func fileMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime().UTC()
}

// unixOrZero converts a unix timestamp, treating 0 as absent.
func unixOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// walkRoot is filepath.WalkDir that also descends a root which is itself a
// symlink -- a projects directory moved to another disk -- where WalkDir alone
// visits nothing while the source still counts as present. Paths are reported
// under root as given, because cursors, meta keys and rewind scopes are keyed
// on it.
func walkRoot(root string, fn fs.WalkDirFunc) error {
	fi, err := os.Lstat(root)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return filepath.WalkDir(root, fn)
	}
	target, err := filepath.EvalSymlinks(root)
	if err != nil {
		return filepath.WalkDir(root, fn)
	}
	return filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(target, path)
		if rerr != nil {
			return fn(path, d, err)
		}
		return fn(filepath.Join(root, rel), d, err)
	})
}

// walkJSONL is the shape almost every adapter needs: walk a set of roots, tail
// each matching file from wherever the last pass stopped, and hand complete
// lines to onLine. A new adapter is then usually a path, a filename predicate
// and a function that turns one line into an Event.
func walkJSONL(
	ctx context.Context,
	c *Ctx,
	roots []string,
	match func(path string) bool,
	onLine func(path string, at int64, line []byte),
) (Result, error) {
	var res Result
	for _, root := range roots {
		err := walkRoot(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !match(path) {
				return nil //nolint:nilerr // one unreadable file must not abort the walk
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}

			n, offset, size, terr := tailJSONL(ctx, c.Store, path, func(at int64, line []byte) { onLine(path, at, line) })
			if terr != nil {
				res.Errors = append(res.Errors, terr)
				// Nothing is committed, so the cursor stays where it was and
				// the file is retried whole on the next pass.
				c.discard()
				res.Files++
				return nil
			}

			// Rows and cursor move together: an interrupted pass either keeps
			// both or neither, and never advances past events it did not store.
			stored, cerr := c.commitFile(ctx, path, n, offset, size)
			if cerr != nil {
				res.Errors = append(res.Errors, cerr)
			}
			res.Stored += stored
			res.Files++
			res.BytesRead += n
			return nil
		})
		if err != nil {
			res.Unparsed = c.takeUnparsed()
			return res, err
		}
	}
	res.Unparsed = c.takeUnparsed()
	return res, nil
}

// hasExt reports whether path ends in suffix, for use as a walkJSONL match.
func hasExt(suffix string) func(string) bool {
	return func(p string) bool { return strings.HasSuffix(p, suffix) }
}

// baseName reports whether the file is named exactly name.
func baseName(name string) func(string) bool {
	return func(p string) bool { return filepath.Base(p) == name }
}

// anyOf combines matchers.
func anyOf(ms ...func(string) bool) func(string) bool {
	return func(p string) bool {
		for _, m := range ms {
			if m(p) {
				return true
			}
		}
		return false
	}
}

// pickTime takes whichever timestamp form a record carries: harnesses disagree
// on RFC3339 strings versus unix seconds, sometimes within one format.
func pickTime(rfc3339 string, unixSec int64) time.Time {
	if t := parseTS(rfc3339); !t.IsZero() {
		return t
	}
	return unixOrZero(unixSec)
}
