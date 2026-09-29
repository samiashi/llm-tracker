package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// The collector's job runs `run` on its own data directory, at background
// priority, logging where `install` and `status` say it does.
func TestTheCollectorsJobRunsItsDataDirectoryInTheBackground(t *testing.T) {
	j := collectorJob("/srv/tracker/bin/llm-tracker-agent", "/srv/tracker")
	if want := []string{"/srv/tracker/bin/llm-tracker-agent", "run", "-data", "/srv/tracker"}; !slices.Equal(j.Args, want) {
		t.Errorf("Args = %q, want %q", j.Args, want)
	}
	if !j.Background {
		t.Error("the collector's job is not at background priority")
	}
	if j.Log != filepath.Join("/srv/tracker", "agent.log") {
		t.Errorf("Log = %q, want the agent.log that install names", j.Log)
	}
}
