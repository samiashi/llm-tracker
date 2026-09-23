package launchd

import (
	"os"
	"path/filepath"
	"testing"
)

// The plist is the only record of the installed data directory, so it must
// read back exactly, even a path that is not plain XML text.
func TestTheInstalledDataDirIsReadBackFromThePlist(t *testing.T) {
	for _, dir := range []string{
		"/Users/dev/.llm-tracker",
		"/Volumes/R&D <shared>/tracker",
	} {
		if got := dataDirIn([]byte(plist("/Users/dev/.llm-tracker/bin/llm-tracker-agent", dir))); got != dir {
			t.Errorf("read back %q from the plist written for %q", got, dir)
		}
	}
	if got := dataDirIn([]byte("not a plist")); got != "" {
		t.Errorf("read %q out of something that is not a plist", got)
	}

	// Through the installed file, in a home of the test's own.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := InstalledDataDir(); got != "" {
		t.Errorf("InstalledDataDir() = %q with nothing installed, want \"\"", got)
	}
	p, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(plist("/opt/agent", "/srv/tracker")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InstalledDataDir(); got != "/srv/tracker" {
		t.Errorf("InstalledDataDir() = %q, want the installed job's /srv/tracker", got)
	}
}
