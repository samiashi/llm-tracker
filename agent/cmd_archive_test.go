package main

import (
	"strings"
	"testing"
	"time"
)

// `sync` also needs the collector stopped, so "run sync" alone leaves the
// re-queued archive waiting on a collector nobody restarts.
func TestResendSaysHowTheQueueIsDelivered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	seed(t, dir, event("a", time.Now(), "m"))

	out, err := agentRun(t, "resend", "-data", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "llm-tracker-agent install -data "+dir) {
		t.Errorf("resend printed %q, want how to start the collector again", out)
	}
}

// resync deletes before it re-reads, so it warns for every harness that can
// lose records -- not only Claude Code: Codex and opencode sessions, Cline
// tasks and Continue logs can all be deleted by the user.
func TestResyncWarnsForEveryHarnessThatCanLoseRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, source := range []string{"claude_code", "cowork", "codex", "opencode", "cline", "continue"} {
		out, err := agentRun(t, "resync", "-data", t.TempDir(), "-source", source, "-yes")
		if err != nil {
			t.Fatalf("resync %s: %v", source, err)
		}
		if !strings.Contains(out, "does not keep every record") {
			t.Errorf("resync %s gave no warning that deleted records cannot be re-read:\n%s", source, out)
		}
	}
}
