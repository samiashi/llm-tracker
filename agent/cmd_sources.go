package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/samiashi/llm-tracker/agent/internal/sources"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// adapterNamed resolves a source name against the registry, naming every
// registered source when it cannot: only a registered adapter has roots to
// probe and a scope to rewind. how says where the name goes on the command
// line.
func adapterNamed(name, how string) (sources.Adapter, error) {
	if a, ok := sources.Lookup(name); ok {
		return a, nil
	}
	registered := strings.Join(sources.Names(), ", ")
	if name == "" {
		return nil, fmt.Errorf("name a source with %s (registered: %s)", how, registered)
	}
	return nil, fmt.Errorf("unknown source %q (registered: %s)", name, registered)
}

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
	ad, err := adapterNamed(name, "probe <source>")
	if err != nil {
		return err
	}

	fmt.Printf("probing %s\n\nroots:\n", ad.Name())
	present := sources.AbsRoots(ad, &sources.Ctx{Home: home})
	for _, r := range ad.Roots() {
		p := filepath.Join(home, r)
		state := "missing"
		if slices.Contains(present, p) {
			state = "found"
		}
		fmt.Printf("  [%s] %s\n", state, p)
	}
	if len(present) == 0 {
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
	if rerr := st.EachPayload(context.Background(), func(raw json.RawMessage) error {
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

	parsed := strconv.Itoa(c.Emitted)
	if distinct != c.Emitted {
		parsed += fmt.Sprintf(" (%d distinct)", distinct)
	}
	fmt.Printf("\nfiles matched: %d (%s read)\n", res.Files, humanBytes(res.BytesRead))
	fmt.Printf("events parsed: %s\nerrors:        %d\n", parsed, len(res.Errors))
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
