package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/samiashi/llm-tracker/agent/internal/sources"
)

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
	ad, err := adapterNamed(source, "-source <name>")
	if err != nil {
		return err
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

// cmdResync discards a source's rows and re-reads it from scratch.
func cmdResync(dataDir, home, source string, yes bool) error {
	ad, err := adapterNamed(source, "-source <name>")
	if err != nil {
		return err
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
		fmt.Println("  transcripts after 30 days, and others let you delete sessions, tasks")
		fmt.Println("  or logs.")
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
