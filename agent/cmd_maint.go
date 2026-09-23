package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/samiashi/llm-tracker/agent/internal/sources"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// cmdSources lists the registered adapters and whether each is present here.
func cmdSources(home string) error {
	c := &sources.Ctx{Home: home}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SOURCE\tPRESENT\tROOTS")
	for _, a := range sources.All() {
		present := "no"
		if sources.Available(a, c) {
			present = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", a.Name(), present, strings.Join(a.Roots(), ", "))
	}
	return w.Flush()
}

// cmdProbe dry-runs one adapter and reports what it would extract, to verify
// one written from documentation on a machine that has the harness. It reads
// every file from the start into a throwaway store, leaving the real cursors
// alone, and prints counts and model names -- never message content.
func cmdProbe(home, name string) error {
	if name == "" {
		return fmt.Errorf("usage: llm-tracker-agent probe <source>  (one of: %s)",
			strings.Join(sources.Names(), ", "))
	}
	ad, ok := sources.Lookup(name)
	if !ok {
		return fmt.Errorf("unknown source %q (registered: %s)", name, strings.Join(sources.Names(), ", "))
	}

	fmt.Printf("probing %s\n\nroots:\n", ad.Name())
	anyRoot := false
	for _, r := range ad.Roots() {
		p := filepath.Join(home, r)
		state := "missing"
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			state, anyRoot = "found", true
		}
		fmt.Printf("  [%s] %s\n", state, p)
	}
	if !anyRoot {
		fmt.Println("\nNothing to read: this harness is not installed here.")
		return nil
	}

	tmp, err := os.MkdirTemp("", "llm-tracker-probe-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	st, err := store.Open(filepath.Join(tmp, "probe.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	c := &sources.Ctx{Store: st, MachineID: "probe", Home: home, Accounts: map[string]*schema.Account{}}
	res, err := ad.Collect(context.Background(), c)
	if _, cerr := c.CommitPending(context.Background()); cerr != nil && err == nil {
		err = cerr
	}

	// Parsed events are counted by the pass, and tokens totalled over every
	// stored row: adapters commit as they go, so the buffer is empty by now. A
	// streamed response is parsed once per line and stored once, so the two
	// counts can differ.
	models := map[string]int64{}
	var total int64
	var distinct int
	var sample *schema.Event
	if rerr := st.EachPayload(context.Background(), "event", func(raw json.RawMessage) error {
		var e schema.Event
		if json.Unmarshal(raw, &e) == nil {
			if sample == nil {
				sample = &e
			}
			distinct++
			models[e.Model] += e.Usage.TotalTokens()
			total += e.Usage.TotalTokens()
		}
		return nil
	}); rerr != nil {
		return rerr
	}

	parsed := strconv.Itoa(c.Totals.Events)
	if distinct != c.Totals.Events {
		parsed += fmt.Sprintf(" (%d distinct)", distinct)
	}
	fmt.Printf("\nfiles matched: %d (%s read)\n", res.Files, humanBytes(res.BytesRead))
	fmt.Printf("events parsed: %s\nquota samples: %d\nerrors:        %d\n",
		parsed, c.Totals.Quota, len(res.Errors))
	if err != nil {
		fmt.Println("adapter error:", err)
	}
	for _, e := range res.Errors {
		fmt.Println("  file error:", e)
	}

	if sample == nil {
		fmt.Println("\nNo usage records recognised.")
		fmt.Println("If the harness is genuinely in use, the on-disk format has moved:")
		fmt.Println("compare a session file against the field names in the adapter.")
		return nil
	}

	fmt.Printf("\ntokens: %s across %d model(s)\n", humanTokens(total), len(models))
	for m, n := range models {
		fmt.Printf("  %-28s %s\n", m, humanTokens(n))
	}
	e := *sample
	fmt.Printf("\nsample event: model=%s provider=%s effort=%q in=%d out=%d cache_read=%d ts=%s\n",
		e.Model, e.Provider, e.Effort, e.Usage.InputTokens, e.Usage.OutputTokens,
		e.Usage.CacheReadTokens, e.TS.Format("2006-01-02T15:04:05Z"))
	return nil
}

// cmdResend re-queues the archive for upload.
func cmdResend(dataDir string) error {
	st, release, err := openStoreLocked(dataDir)
	if err != nil {
		return err
	}
	defer release()
	n, err := st.MarkAllUnsent(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("re-queued %d events for upload; the collector sends them on its next pass\n", n)
	fmt.Println("  stopped it to run this? start it again:", agentCmd(dataDir, "install"))
	fmt.Println("  no collector installed? send them now: ", agentCmd(dataDir, "sync"))
	return nil
}

// cmdRewind re-reads a source without discarding anything.
func cmdRewind(dataDir, home, source string) error {
	if source == "" {
		return fmt.Errorf("-source is required (%s)", strings.Join(sources.Names(), " | "))
	}
	ad, ok := sources.Lookup(source)
	if !ok {
		return fmt.Errorf("unknown source %q", source)
	}
	st, release, err := openStoreLocked(dataDir)
	if err != nil {
		return err
	}
	defer release()

	n, err := st.ResetCursors(context.Background(), sources.ScopeOf(ad, home))
	if err != nil {
		return err
	}
	fmt.Printf("rewound %d file cursors for %s\n", n, source)
	fmt.Println("the next scan re-reads them; existing rows are upgraded, not replaced")
	return nil
}

func cmdResync(dataDir, home, source string, yes bool) error {
	if source == "" {
		return fmt.Errorf("-source is required (%s)", strings.Join(sources.Names(), " | "))
	}
	// Validated against the registry: only a registered adapter has a scope.
	ad, ok := sources.Lookup(source)
	if !ok {
		return fmt.Errorf("unknown source %q (registered: %s)", source, strings.Join(sources.Names(), ", "))
	}
	st, release, err := openStoreLocked(dataDir)
	if err != nil {
		return err
	}
	defer release()

	// Say what will be destroyed, before destroying it: `resync` is easily
	// mistaken for `rewind`, and for a harness that deletes its own
	// transcripts its damage cannot be undone by any amount of re-reading.
	n, err := st.CountSource(context.Background(), source)
	if err != nil {
		return err
	}
	fmt.Printf("resync will DELETE %d stored events for %s and re-read the source from scratch.\n", n, source)
	if !sources.KeepsHistory(ad) {
		fmt.Println()
		fmt.Println("  This harness does not keep every record: Claude Code and Cowork delete")
		fmt.Println("  transcripts after 30 days, and others let you delete tasks or logs.")
		fmt.Println("  Anything no longer on disk exists only in this archive and CANNOT be")
		fmt.Println("  re-read. It will be lost, not rebuilt.")
		fmt.Println()
		fmt.Println("  `rewind` re-reads without deleting, and is almost always what you want.")
	}

	if !confirm(fmt.Sprintf("Type %q to continue: ", source), source, yes) {
		fmt.Println("aborted; nothing was deleted")
		return nil
	}

	n, err = st.ResetSource(context.Background(), source, sources.ScopeOf(ad, home))
	if err != nil {
		return err
	}
	fmt.Printf("cleared %d local events for %s; next scan will re-read it\n", n, source)
	return nil
}

// confirm reads a line from stdin and reports whether it matched want. With no
// terminal it refuses rather than assuming yes: a script must pass -yes.
func confirm(prompt, want string, yes bool) bool {
	if yes {
		return true
	}
	if !isTerminal(os.Stdin) {
		fmt.Println("refusing to delete without a terminal to confirm at; pass -yes if you mean it")
		return false
	}
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(line) == want
}

// isTerminal reports whether f is attached to a character device.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
