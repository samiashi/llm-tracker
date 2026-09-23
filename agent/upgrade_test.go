package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The only check between a compromised release and code running as the
// developer.
func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	if err := os.WriteFile(bin, []byte("pretend this is a mach-o binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	const good = "44114b33ca4291a261276eccce1d38558492adf70b41140254054f21d853b2cc"

	sums := filepath.Join(dir, "agent.sha256")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(sums, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(strings.Repeat("0", 64) + "  llm-tracker-agent-darwin-arm64\n")
	if err := verifySHA256(bin, sums); err == nil {
		t.Fatal("a wrong digest was accepted")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	write("not-a-digest  file\n")
	if err := verifySHA256(bin, sums); err == nil {
		t.Fatal("a malformed checksum file was accepted")
	}

	if err := verifySHA256(bin, filepath.Join(dir, "missing.sha256")); err == nil {
		t.Fatal("a missing checksum file was accepted")
	}

	write(good + "  llm-tracker-agent-darwin-arm64\n")
	if err := verifySHA256(bin, sums); err != nil {
		t.Fatalf("the correct digest was rejected: %v", err)
	}
}

func TestUpgradeNeverMovesAReleaseBackwards(t *testing.T) {
	for _, tc := range []struct {
		running, latest string
		force           bool
		replace, err    bool
	}{
		{"v1.4.0", "v1.4.1", false, true, false},
		{"v1.4.0", "v2.0.0", false, true, false},
		{"v1.4.0", "v1.4.0", false, false, false},
		{"v1.4.0", "1.4.0", false, false, false},
		{"v1.4.0", "v1.3.9", false, false, true},
		{"v1.10.0", "v1.9.0", false, false, true},
		{"v1.4.0", "v1.3.9", true, true, false},
		{"v1.4.0", "abc1234-dirty", false, false, true},
		{"dev", "v1.3.9", false, true, false},
		{"6387414-dirty", "v1.3.9", false, true, false},
	} {
		replace, err := upgradeVerdict(tc.running, tc.latest, tc.force)
		if replace != tc.replace || (err != nil) != tc.err {
			t.Errorf("%s -> %s (force %v): replace=%v err=%v, want replace=%v err=%v",
				tc.running, tc.latest, tc.force, replace, err, tc.replace, tc.err)
		}
	}
}

// Restarted on the default, a collector installed with -data comes back on a
// store with no configuration and stops uploading.
func TestUpgradeRestartsTheCollectorOnItsOwnDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := collectorDataDir("", home); got != filepath.Join(home, ".llm-tracker") {
		t.Errorf("with nothing installed: %q, want the default", got)
	}

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.samiashi.llm-tracker</string>
  <key>ProgramArguments</key>
  <array>
    <string>/srv/tracker/bin/llm-tracker-agent</string>
    <string>run</string>
    <string>-data</string>
    <string>/srv/tracker</string>
  </array>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(agents, "io.github.samiashi.llm-tracker.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := collectorDataDir("", home); got != "/srv/tracker" {
		t.Errorf("installed with -data /srv/tracker: %q, want the installed directory", got)
	}
	if got := collectorDataDir("/elsewhere", home); got != "/elsewhere" {
		t.Errorf("with -data given: %q, want the flag's directory", got)
	}
}
