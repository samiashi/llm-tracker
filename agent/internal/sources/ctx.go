// Package sources holds one adapter per harness.
//
// Every adapter is deliberately shallow: find the records that carry token
// counts, map them onto schema.Event, and ignore everything else. None of them
// read message text. The formats are private, undocumented and change without
// notice, so adapters are expected to break, and the collector's job is to
// notice loudly rather than quietly report less.
package sources

import (
	"context"
	"time"

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
	Accounts map[string]*schema.Account
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
