package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// Every way of starting the server acts on one database, the installed
// job's: one started by hand on another would take the collector's uploads
// on the same port, and they would never reach the installed server.
func TestEveryCommandActsOnTheInstalledServersDatabase(t *testing.T) {
	home := "/Users/dev"
	job := serverJob("/Users/dev/.llm-tracker-server/llm-tracker-server", home, 0)
	i := slices.Index(job.Args, "-db")
	if i < 0 || i+1 == len(job.Args) {
		t.Fatalf("the job names no database: %q", job.Args)
	}
	installed := job.Args[i+1]
	if installed != filepath.Join(home, ".llm-tracker-server", "llm-tracker.db") {
		t.Errorf("the job's database is %q, want it in ~/.llm-tracker-server", installed)
	}
	for _, args := range [][]string{nil, {"-v"}, {"-revoke", "someone"}} {
		if _, o, err := parseArgs(args, home); err != nil || o.db != installed {
			t.Errorf("%q acts on %q (%v), want the installed %q", args, o.db, err, installed)
		}
	}
	if job.Background {
		t.Error("the server's job is at background priority; it answers the dashboard")
	}
}

// install carries -retain into the job and refuses what the job would drop:
// installed with -db, a server would serve somewhere other than asked, and
// nobody would be told.
func TestInstallTakesRetainAndNothingElse(t *testing.T) {
	for _, c := range []struct {
		args []string
		cmd  string
		ok   bool
	}{
		{nil, "", true},
		{[]string{"-db", "/tmp/copy.db", "-addr", "127.0.0.1:8791"}, "", true},
		{[]string{"install"}, "install", true},
		{[]string{"install", "-retain", "30"}, "install", true},
		{[]string{"install", "-db", "/tmp/copy.db"}, "", false},
		{[]string{"install", "-v"}, "", false},
		{[]string{"uninstall"}, "uninstall", true},
		{[]string{"uninstall", "-retain", "30"}, "", false},
		{[]string{"serve"}, "", false},
		{[]string{"-v", "install"}, "", false},
	} {
		cmd, _, err := parseArgs(c.args, "/Users/dev")
		if (err == nil) != c.ok || (c.ok && cmd != c.cmd) {
			t.Errorf("%q: command %q, error %v; want %q, ok=%v", c.args, cmd, err, c.cmd, c.ok)
		}
	}

	if _, o, _ := parseArgs([]string{"install", "-retain", "30"}, "/Users/dev"); o.retain != 30 {
		t.Fatalf("install -retain 30 parsed as %d", o.retain)
	}
	kept := serverJob("/bin/server", "/Users/dev", 30).Args
	if i := slices.Index(kept, "-retain"); i < 0 || i+1 == len(kept) || kept[i+1] != "30" {
		t.Errorf("installed with -retain 30, the job runs %q", kept)
	}
	if all := serverJob("/bin/server", "/Users/dev", 0).Args; slices.Contains(all, "-retain") {
		t.Errorf("installed without -retain, the job runs %q", all)
	}
}
