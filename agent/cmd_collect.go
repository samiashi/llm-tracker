package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/collect"
	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/agent/internal/tracker"
	"github.com/samiashi/llm-tracker/schema"
)

func cmdScan(dataDir, home string, log *slog.Logger) error {
	st, release, err := openStoreLocked(dataDir)
	if err != nil {
		return err
	}
	defer release()

	ctx := context.Background()
	sum, err := collect.Run(ctx, st, home, log)
	if err != nil {
		return err
	}
	printSummary(sum)

	events, unsent, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\npass took %s | archive: %d events (%d awaiting upload) | unsupported harnesses: %d\n",
		sum.Duration.Round(time.Millisecond), events, unsent, sum.Unknown)
	return nil
}

func printSummary(sum *collect.Summary) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SOURCE\tSTATUS\tFILES\tREAD\tFOUND\tNEW\tUNPARSED\tERR")
	names := make([]string, 0, len(sum.PerSource))
	for k := range sum.PerSource {
		names = append(names, string(k))
	}
	sort.Strings(names)
	for _, n := range names {
		s := sum.PerSource[schema.Source(n)]
		status := "absent"
		if s.Available {
			status = "ok"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%d\t%d\t%d\n",
			n, status, s.Files, humanBytes(s.BytesRead), s.Found, s.Stored,
			s.Unparsed, s.Errors)
	}
	_ = w.Flush()
}

func cmdSync(dataDir string, log *slog.Logger) error {
	cfg, err := config.Load(dataDir)
	if err != nil {
		return err
	}
	if cfg.ServerURL == "" {
		return fmt.Errorf("not enrolled with a tracker yet (run: %s)", agentCmd(dataDir, "enroll"))
	}
	st, release, err := openStoreLocked(dataDir)
	if err != nil {
		return err
	}
	defer release()

	ctx := context.Background()
	machineID, err := identity.MachineID(ctx, st)
	if err != nil {
		return err
	}
	c := tracker.New(cfg.ServerURL, cfg.Token, version, log)
	stats, err := c.Push(ctx, st, machineID)
	// Recorded as the daemon records its own, or `status` says "never"
	// straight after a manual sync.
	if rerr := st.RecordSync(ctx, stats.Events, err); rerr != nil {
		log.Warn("recording sync health", "err", rerr)
	}
	if err != nil {
		return err
	}
	fmt.Printf("uploaded %d events and %d unsupported harnesses in %d batch(es)\n",
		stats.Events, stats.Unknown, stats.Batches)
	if stats.Rejected > 0 {
		fmt.Printf("the server rejected %d of those events as implausible\n", stats.Rejected)
	}
	if stats.Skipped > 0 {
		fmt.Printf("the server refused %d events that predate its retention window; "+
			"%d are kept locally and no longer offered\n", stats.Skipped, stats.Retired)
	}
	return nil
}

// passInterval is how often the daemon collects and uploads.
const passInterval = 5 * time.Minute

// cmdRun is the long-running mode launchd starts.
//
// It stays in the foreground: launchd tracks the process it spawns, so one
// that daemonised itself would be considered dead and restarted forever.
func cmdRun(dataDir, home string, log *slog.Logger) error {
	// Taken before the store is opened, so a second agent fails with an
	// explanation rather than by contending for the writer (see store.Acquire).
	lock, err := store.Acquire(dataDir)
	if err != nil {
		return err
	}
	defer lock.Release() //nolint:errcheck

	cfg, err := config.Load(dataDir)
	if err != nil {
		return err
	}
	st, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() { <-stop; cancel() }()

	machineID, err := identity.MachineID(ctx, st)
	if err != nil {
		return err
	}
	var client *tracker.Client
	if cfg.ServerURL != "" {
		client = tracker.New(cfg.ServerURL, cfg.Token, version, log)
	}

	log.Info("agent started", "interval", passInterval, "server", cfg.ServerURL, "machine", machineID)

	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	silent := silence{}
	for {
		sum, err := collect.Run(ctx, st, home, log)
		if err != nil && ctx.Err() == nil {
			log.Error("collection pass failed", "err", err)
		} else if sum != nil {
			log.Info("pass complete", "dur", sum.Duration, "unknown", sum.Unknown,
				"found", sum.Found(), "stored", sum.Stored())
			silent.report(sum, log)
			if rerr := st.RecordPass(ctx, sum.Found(), sum.Stored()); rerr != nil {
				log.Warn("recording pass health", "err", rerr)
			}
		}

		// Config is re-read each pass, so `enroll` takes effect on a running
		// daemon: held from startup, a replaced token would keep the agent
		// 401ing until somebody knew to restart it.
		if fresh, ferr := config.Load(dataDir); ferr == nil &&
			(fresh.ServerURL != cfg.ServerURL || fresh.Token != cfg.Token) {
			log.Info("configuration changed, reloading", "server", fresh.ServerURL)
			cfg = fresh
			client = nil
			if cfg.ServerURL != "" {
				client = tracker.New(cfg.ServerURL, cfg.Token, version, log)
			}
		}

		// A sync failure is not fatal: the archive keeps accruing locally and
		// the next pass retries.
		if client != nil && ctx.Err() == nil {
			stats, err := client.Push(ctx, st, machineID)
			if rerr := st.RecordSync(ctx, stats.Events, err); rerr != nil {
				log.Warn("recording sync health", "err", rerr)
			}
			if err != nil {
				// A rejected credential never fixes itself, so it is logged
				// at Error with the remedy rather than retried quietly.
				if tracker.AuthRejected(err.Error()) {
					log.Error("the server rejected this agent's token; nothing will upload until it is fixed",
						"err", err,
						"fix", agentCmd(dataDir, "enroll"))
				} else {
					log.Warn("sync failed, will retry", "err", err)
				}
			} else if stats.Events > 0 {
				log.Info("synced", "events", stats.Events, "batches", stats.Batches)
			}
		}

		select {
		case <-ctx.Done():
			log.Info("agent stopped")
			return nil
		case <-ticker.C:
		}
	}
}

// silenceAlarmPasses is how many passes with new data and no usage a source
// needs before it is reported. One is ordinary: Codex writes a turn's tool
// calls and output before the usage line that closes it, so a pass landing
// mid-turn reads bytes with no usage in them. A format that moved under its
// adapter stays silent every time new data arrives.
const silenceAlarmPasses = 3

// silence counts, per source, passes that read new data and understood none
// of it (see SourceSummary.Silent) since the source last produced usage. A
// pass with nothing new to read changes nothing. The daemon never runs `scan`,
// whose table is the only other place a silent source shows.
type silence map[schema.Source]int

// report logs, at Error, every source silent for silenceAlarmPasses or more.
func (sl silence) report(sum *collect.Summary, log *slog.Logger) {
	names := make([]string, 0, len(sum.PerSource))
	for name := range sum.PerSource {
		names = append(names, string(name))
	}
	sort.Strings(names)

	for _, name := range names {
		src := schema.Source(name)
		s := sum.PerSource[src]
		switch {
		case s.Found > 0:
			delete(sl, src)
			continue
		case !s.Silent():
			continue
		}
		sl[src]++
		if sl[src] < silenceAlarmPasses {
			continue
		}
		log.Error("source produced no usage from new data -- its format may have changed",
			"source", name, "passes", sl[src], "files", s.Files, "bytes_read", s.BytesRead,
			"records_read", s.Scanned, "unparsed_lines", s.Unparsed, "errors", s.Errors)
	}
}
