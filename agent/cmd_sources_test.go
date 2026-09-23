package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A probe that reads back one page of the store undercounts any real history.
func TestProbeCountsEveryRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "-Users-dev-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	line := func(i int, ts string, input int) {
		fmt.Fprintf(&b, `{"type":"assistant","requestId":"r%d","sessionId":"s","timestamp":%q,`+
			`"message":{"id":"m%d","model":"claude-sonnet-4-5","usage":{"input_tokens":%d}}}`+"\n",
			i, ts, i, input)
	}
	for i := range 5000 {
		line(i, "2026-09-20T10:00:00Z", 10)
	}
	// The latest response carries almost all the tokens, so a probe that stops
	// at the first 5,000 rows shows it. It streams in two lines: two records
	// parsed, one event stored.
	line(5000, "2026-09-21T10:00:00Z", 10)
	line(5000, "2026-09-21T10:00:00Z", 1_000_000_000)
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := agentRun(t, "probe", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"events parsed: 5002 (5001 distinct)", "tokens: 1.0B"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe did not report %q:\n%s", want, out)
		}
	}
}

// Every command that takes a source names the ones it would accept, however
// the name is missing or wrong, and where on the command line it goes.
func TestASourceCommandNamesTheRegisteredSources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"probe"}, "probe <source>"},
		{[]string{"probe", "nonsense"}, `unknown source "nonsense"`},
		{[]string{"rewind", "-data", t.TempDir()}, "-source <name>"},
		{[]string{"rewind", "-data", t.TempDir(), "-source", "nonsense"}, `unknown source "nonsense"`},
		{[]string{"resync", "-data", t.TempDir(), "-yes"}, "-source <name>"},
		{[]string{"resync", "-data", t.TempDir(), "-source", "nonsense", "-yes"}, `unknown source "nonsense"`},
	} {
		_, err := agentRun(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.says) ||
			!strings.Contains(err.Error(), "registered: claude_code, cline, codex") {
			t.Errorf("%s: %v; want it to say %q and list the registered sources",
				strings.Join(tc.args, " "), err, tc.says)
		}
	}
}
