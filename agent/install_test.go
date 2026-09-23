package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// serveRelease writes a release for install.sh to download from dir/release:
// for each arch, an agent that records each run in dir/calls as
// "<arch> <args>", and its checksum.
func serveRelease(t *testing.T, dir string) {
	t.Helper()
	release := filepath.Join(dir, "release")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		asset := filepath.Join(release, "llm-tracker-agent-darwin-"+arch)
		agent := "#!/bin/sh\necho \"" + arch + " $*\" >> '" + filepath.Join(dir, "calls") + "'\necho v9.9.9\n"
		sum := sha256.Sum256([]byte(agent))
		if err := os.WriteFile(asset, []byte(agent), 0o755); err != nil {
			t.Fatal(err)
		}
		line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(asset) + "\n"
		if err := os.WriteFile(asset+".sha256", []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runInstallScript runs install.sh as colleagues do, piped to sh, on a stand-in
// Mac: HOME and TMPDIR are temporary, gh serves dir/release, and launchctl only
// records that it was called. It returns the output, every run of a served
// agent and every launchctl call, in order, and the script's error.
func runInstallScript(t *testing.T, dir string) (string, []string, error) {
	t.Helper()
	script, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	home, bin, tmp := filepath.Join(dir, "home"), filepath.Join(dir, "bin"), filepath.Join(dir, "tmp")
	for _, d := range []string{home, bin, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls")
	stubs := map[string]string{
		// install.sh pins GH_HOST itself, so a gh pointed anywhere else fails.
		"gh": `#!/bin/sh
[ "$GH_HOST" = github.com ] || { echo "gh used with GH_HOST=$GH_HOST" >&2; exit 2; }
case "$1 $2" in
"auth status") ;;
"api user") echo tester ;;
"release download")
  while [ $# -gt 0 ]; do
    case "$1" in --pattern) pattern=$2; shift ;; --output) output=$2; shift ;; esac
    shift
  done
  cp "` + filepath.Join(dir, "release") + `/$pattern" "$output" ;;
*) echo "unexpected: gh $*" >&2; exit 2 ;;
esac
`,
		"launchctl": "#!/bin/sh\necho \"launchctl $*\" >> '" + calls + "'\nexit 1\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("/bin/sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.CombinedOutput()
	logged, _ := os.ReadFile(calls)
	var runs []string
	if s := strings.TrimSpace(string(logged)); s != "" {
		runs = strings.Split(s, "\n")
	}
	return string(out), runs, err
}

// install.sh is how every colleague gets the agent, and it promises to run
// nothing it has not checked against the release's published checksum.
func TestInstallScriptRunsNothingItCouldNotVerify(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("install.sh is macOS only")
	}
	for _, tc := range []struct {
		name    string
		alter   func(asset string) error // the release, after it was checksummed
		refusal string
	}{
		{"a verified download is installed", nil, ""},
		{"a download altered after it was checksummed never runs", func(asset string) error {
			f, err := os.OpenFile(asset, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString("# altered\n")
			return err
		}, "Checksum mismatch"},
		{"a download with no published checksum never runs", func(asset string) error {
			return os.Remove(asset + ".sha256")
		}, "refusing to install an unverified binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			serveRelease(t, dir)
			if tc.alter != nil {
				for _, arch := range []string{"arm64", "amd64"} {
					if err := tc.alter(filepath.Join(dir, "release", "llm-tracker-agent-darwin-"+arch)); err != nil {
						t.Fatal(err)
					}
				}
			}

			out, runs, err := runInstallScript(t, dir)
			_, missing := os.Stat(filepath.Join(dir, "home", ".local", "bin", "llm-tracker-agent"))
			if tc.refusal != "" {
				if err == nil || !strings.Contains(out, tc.refusal) || len(runs) > 0 || missing == nil {
					t.Fatalf("want a refusal naming %q, nothing run and nothing installed; got error %v, runs %q\n%s",
						tc.refusal, err, runs, out)
				}
				return
			}
			if err != nil || missing != nil {
				t.Fatalf("not installed: %v, %v\n%s", err, missing, out)
			}
			// The agent is asked for its version, then to enrol and to install
			// itself; a launchctl call from the script would show up here too.
			var cmds []string
			for _, r := range runs {
				_, cmd, _ := strings.Cut(r, " ")
				cmds = append(cmds, cmd)
			}
			if want := []string{"version", "enroll", "install"}; !slices.Equal(cmds, want) {
				t.Errorf("runs %q, want the agent asked to %q", runs, want)
			}
		})
	}
}
