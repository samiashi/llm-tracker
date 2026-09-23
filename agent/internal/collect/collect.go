// Package collect runs every adapter and persists what they find.
package collect

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/sources"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// Summary is one pass, printed by `scan` and logged by the daemon.
type Summary struct {
	StartedAt time.Time
	Duration  time.Duration
	// PerSource is keyed by source name, absent sources included.
	PerSource map[schema.Source]SourceSummary
	Unknown   int
}

// Found totals the events parsed across every source this pass.
func (s *Summary) Found() int {
	n := 0
	for _, ss := range s.PerSource {
		n += ss.Found
	}
	return n
}

// Stored totals the events written across every source this pass.
func (s *Summary) Stored() int {
	n := 0
	for _, ss := range s.PerSource {
		n += ss.Stored
	}
	return n
}

type SourceSummary struct {
	Available bool
	Files     int
	BytesRead int64
	// Scanned counts records read from somewhere that is not a file, so a
	// database-backed adapter can still say it read something.
	Scanned int
	Found   int
	Stored  int
	Errors  int
	// Unparsed counts lines read and not understood.
	Unparsed int
}

// Silent reports a source that read new data and understood none of it, which
// is how a format moving under its adapter looks: nothing fails, the numbers
// just taper away. Bytes or rows read, not files matched -- a caught-up source
// matches every file and reads nothing.
func (s SourceSummary) Silent() bool {
	return s.Available && (s.BytesRead > 0 || s.Scanned > 0) && s.Found == 0
}

const (
	// versionKey records the collector version whose upgrade work is done.
	versionKey = "collector:version"
	// dedupeKey lists sources whose re-keyed rows are still to be paired with
	// their replacements (see dedupeSuperseded).
	dedupeKey = "collector:dedupe"
)

// backfillIfUpgraded purges and re-reads the sources a collector upgrade
// invalidated, before the pass reads anything.
//
// The version is recorded only once every purge and cursor reset has
// succeeded, so a failure or a kill part-way is redone on the next pass rather
// than leaving rows that nothing will purge or re-read.
func backfillIfUpgraded(ctx context.Context, st *store.Store, home string, log *slog.Logger) {
	var stored int
	if v, err := st.Meta(ctx, versionKey); err == nil && v != "" {
		stored, _ = strconv.Atoi(v)
	}

	// A downgrade records the running version, so rolling forward again sees
	// the gap and re-reads what this build collected; left at the newer
	// value, those rows keep this build's readings for good.
	if stored > sources.CollectorVersion {
		log.Warn("collector downgraded; rows collected from here need a backfill "+
			"when it is rolled forward again",
			"stored", stored, "running", sources.CollectorVersion)
		if err := st.SetMeta(ctx, versionKey, strconv.Itoa(sources.CollectorVersion)); err != nil {
			log.Warn("backfill: recording collector version", "err", err)
		}
		return
	}
	if stored == sources.CollectorVersion {
		return
	}

	failed := false
	// Purges first, and for a store with no recorded version too: it reads back
	// as zero, and those are the stores holding superseded rows. Rows no
	// re-read will reach would otherwise double every token they hold.
	for _, src := range sources.SourcesNeedingPurge(stored) {
		ad, ok := sources.Lookup(string(src))
		if !ok {
			log.Warn("backfill: no adapter for a source the upgrade names", "source", src)
			continue
		}
		n, err := st.ResetSource(ctx, string(src), sources.ScopeOf(ad, home))
		if err != nil {
			log.Warn("backfill: discarding superseded rows; retrying next pass", "source", src, "err", err)
			failed = true
			continue
		}
		if n > 0 {
			log.Info("collector replaced this source's events, discarding the old ones",
				"source", src, "from", stored, "to", sources.CollectorVersion,
				"rows_discarded", n)
		}
	}

	for _, src := range sources.SourcesNeedingBackfill(stored) {
		ad, ok := sources.Lookup(string(src))
		if !ok {
			log.Warn("backfill: no adapter for a source the upgrade names", "source", src)
			continue
		}
		n, err := st.ResetCursors(ctx, sources.ScopeOf(ad, home))
		if err != nil {
			log.Warn("backfill: resetting cursors; retrying next pass", "source", src, "err", err)
			failed = true
			continue
		}
		if n > 0 || stored > 0 {
			log.Info("collector upgraded, re-reading source",
				"source", src, "from", stored, "to", sources.CollectorVersion,
				"cursors_reset", n)
		}
	}
	// Recorded before the version, so a kill between the two still leaves the
	// dedupe to do; it runs after the pass that re-reads the replacements.
	if pending := addDedupe(ctx, st, sources.SourcesNeedingDedupe(stored)); pending != nil {
		log.Warn("backfill: recording sources to dedupe; retrying next pass", "err", pending)
		failed = true
	}
	if failed {
		return
	}
	if err := st.SetMeta(ctx, versionKey, strconv.Itoa(sources.CollectorVersion)); err != nil {
		log.Warn("backfill: recording collector version", "err", err)
	}
}

// addDedupe adds sources to the pending dedupe list.
func addDedupe(ctx context.Context, st *store.Store, srcs []schema.Source) error {
	if len(srcs) == 0 {
		return nil
	}
	pending := pendingDedupe(ctx, st)
	for _, s := range srcs {
		if !slices.Contains(pending, string(s)) {
			pending = append(pending, string(s))
		}
	}
	return st.SetMeta(ctx, dedupeKey, strings.Join(pending, ","))
}

func pendingDedupe(ctx context.Context, st *store.Store) []string {
	v, err := st.Meta(ctx, dedupeKey)
	if err != nil || v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// dedupeSuperseded deletes the rows a re-keyed source's re-read replaced, as
// sources.Superseded pairs them: an old row goes only once its replacement is
// stored, so a record since removed from disk keeps the only row it has. A
// source stays pending until a pass reads it without errors, since a partial
// re-read has not written every replacement yet.
func dedupeSuperseded(ctx context.Context, st *store.Store, sum *Summary, log *slog.Logger) {
	var still []string
	for _, src := range pendingDedupe(ctx, st) {
		payloads, err := st.SourcePayloads(ctx, src)
		if err != nil {
			log.Warn("dedupe: reading stored events", "source", src, "err", err)
			still = append(still, src)
			continue
		}
		events := make([]schema.Event, 0, len(payloads))
		for _, p := range payloads {
			var e schema.Event
			if json.Unmarshal(p, &e) == nil {
				events = append(events, e)
			}
		}
		n, err := st.DeleteEvents(ctx, sources.Superseded(schema.Source(src), events))
		if err != nil {
			log.Warn("dedupe: discarding superseded rows", "source", src, "err", err)
			still = append(still, src)
			continue
		}
		if n > 0 {
			log.Info("discarded rows an upgrade re-keyed", "source", src, "rows", n)
		}
		if sum.PerSource[schema.Source(src)].Errors > 0 {
			still = append(still, src)
		}
	}
	if err := st.SetMeta(ctx, dedupeKey, strings.Join(still, ",")); err != nil {
		log.Warn("dedupe: recording what is left", "err", err)
	}
}

// Run executes one collection pass over every registered adapter.
//
// A failing adapter is logged and skipped rather than aborting the pass: one
// harness changing its format must not stop the others from reporting.
func Run(ctx context.Context, st *store.Store, home string, log *slog.Logger) (*Summary, error) {
	backfillIfUpgraded(ctx, st, home, log)

	machineID, err := identity.MachineID(ctx, st)
	if err != nil {
		return nil, err
	}

	accounts := map[string]*schema.Account{}
	for _, a := range identity.All() {
		accounts[a.Provider] = a
		// Recorded before anything is read, so this pass's events are
		// attributed against a timeline that includes any switch since the
		// last one.
		if err := st.RecordActiveAccount(ctx, a.Provider, a.Ref); err != nil {
			log.Warn("record active account", "provider", a.Provider, "err", err)
		}
	}

	history, err := st.AccountWindows(ctx)
	if err != nil {
		return nil, err
	}

	c := &sources.Ctx{
		Store: st, MachineID: machineID, Home: home,
		Accounts: accounts, AccountHistory: history,
	}
	sum := &Summary{StartedAt: time.Now(), PerSource: map[schema.Source]SourceSummary{}}

	for _, ad := range sources.All() {
		name := ad.Name()
		ss := SourceSummary{Available: sources.Available(ad, c)}
		if !ss.Available {
			sum.PerSource[name] = ss
			continue
		}

		res, err := ad.Collect(ctx, c)
		if err != nil {
			log.Error("adapter failed", "source", name, "err", err)
			ss.Errors++
		}

		// Flushes what a database-backed adapter emitted outside a file walk;
		// the walk commits per file itself (see store.CommitFile).
		pending, perr := c.CommitPending(ctx)
		if perr != nil {
			log.Error("store events", "source", name, "err", perr)
			ss.Errors++
		}

		ss.Files, ss.BytesRead, ss.Scanned = res.Files, res.BytesRead, res.Scanned
		ss.Stored = res.Stored + pending
		ss.Found, c.Emitted = c.Emitted, 0
		ss.Errors += len(res.Errors)
		ss.Unparsed = res.Unparsed
		for _, e := range res.Errors {
			log.Warn("file skipped", "source", name, "err", e)
		}

		sum.PerSource[name] = ss
	}

	if ctx.Err() == nil {
		dedupeSuperseded(ctx, st, sum, log)
	}

	if err := st.ClearUnknown(ctx); err != nil {
		log.Warn("clear unknown sources", "err", err)
	}
	if n, err := sources.ScanUnknown(ctx, c); err != nil {
		log.Warn("scan unknown sources", "err", err)
	} else {
		sum.Unknown = n
	}

	sum.Duration = time.Since(sum.StartedAt)
	return sum, nil
}
