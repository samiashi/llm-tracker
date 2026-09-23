package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A collector installed with -data is fixed by an install on that directory;
// a bare install would start another on the default one.
func TestStatusSuggestsInstallingOnTheCollectorsOwnDataDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	want := "llm-tracker-agent install -data " + dir
	for name, lines := range map[string][]string{
		"not installed":  daemonVerdict(dir, false, "", errors.New("not loaded")),
		"binary missing": daemonVerdict(dir, true, filepath.Join(dir, "gone"), nil),
	} {
		if got := strings.Join(lines, "\n"); !strings.Contains(got, want) {
			t.Errorf("%s: status said\n%s\nwant %q", name, got, want)
		}
	}
}
