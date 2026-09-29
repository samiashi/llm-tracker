package launchd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The plist is a job's only record of how it runs -- the collector reads its
// data directory back from it -- so its arguments must read back exactly, even
// ones that are not plain XML text.
func TestTheArgumentsAreReadBackFromThePlist(t *testing.T) {
	j := Job{
		Label:  "io.example.test",
		Args:   []string{"/opt/bin/tool", "run", "-data", "/Volumes/R&D <shared>/tracker"},
		Log:    "/tmp/out.log",
		ErrLog: "/tmp/err.log",
	}
	if got := argsIn([]byte(j.plist())); !slices.Equal(got, j.Args) {
		t.Errorf("read back %q from the plist written for %q", got, j.Args)
	}
	if got := argsIn([]byte("not a plist")); got != nil {
		t.Errorf("read %q out of something that is not a plist", got)
	}

	// Through the installed file, in a home of the test's own.
	t.Setenv("HOME", t.TempDir())
	if got := InstalledArgs(j.Label); got != nil {
		t.Errorf("InstalledArgs() = %q with nothing installed, want none", got)
	}
	p, err := plistPath(j.Label)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(j.plist()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InstalledArgs(j.Label); !slices.Equal(got, j.Args) {
		t.Errorf("InstalledArgs() = %q, want the installed job's %q", got, j.Args)
	}
}

// Only a background job gives up CPU and I/O: the server answers the
// dashboard while someone is looking at it.
func TestOnlyABackgroundJobRunsAtLowPriority(t *testing.T) {
	for _, background := range []bool{true, false} {
		p := Job{Label: "io.example.test", Args: []string{"/opt/bin/tool"}, Background: background}.plist()
		for _, key := range []string{"<key>ProcessType</key>", "<key>LowPriorityIO</key>", "<key>Nice</key>"} {
			if strings.Contains(p, key) != background {
				t.Errorf("Background=%v, but the plist has %s: %v", background, key, !background)
			}
		}
	}
}

// The copy is staged and renamed into place, and the staged file never
// outlives the call: whether the install lands or fails, only the binary
// launchd starts is left behind.
func TestInstallBinaryLeavesNoStagedCopyBehind(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(src, []byte("pretend this is a mach-o binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("installed", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "bin", "tool")
		if err := InstallBinary(src, dst); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(dst); err != nil || string(b) != "pretend this is a mach-o binary" {
			t.Fatalf("installed %q (%v), want the running binary's bytes", b, err)
		}
		if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
			t.Fatalf("the staged copy is still there: %v", err)
		}
		// Run from the install location, a reinstall copies the binary onto
		// itself, and must leave it whole.
		if err := InstallBinary(dst, dst); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(dst); string(b) != "pretend this is a mach-o binary" {
			t.Fatalf("reinstalling from the install location left %q", b)
		}
	})

	t.Run("the rename fails", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "bin", "tool")
		// A directory where the binary goes: the rename over it fails.
		if err := os.MkdirAll(filepath.Join(dst, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := InstallBinary(src, dst); err == nil {
			t.Fatal("installed over a directory")
		}
		if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
			t.Fatalf("a failed install left its staged copy: %v", err)
		}
	})
}
