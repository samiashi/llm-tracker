// Package launchd installs this project's binaries as per-user LaunchAgents:
// the collector, and the server behind the dashboard.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Job is one LaunchAgent, started at login and again whenever it exits.
type Job struct {
	Label string
	// Args is the program, then its arguments.
	Args []string
	// Log and ErrLog take the job's stdout and stderr.
	Log, ErrLog string
	// Background lowers the job's CPU and I/O priority.
	Background bool
}

// plistPath is where a LaunchAgent's definition lives. A LaunchAgent rather
// than a LaunchDaemon: each binary serves one person, and has no business
// running as root or touching other accounts on the machine.
func plistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// plist renders the job definition.
func (j Job) plist() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlText(j.Label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range j.Args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlText(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	b.WriteString("  <key>KeepAlive</key><true/>\n")
	if j.Background {
		b.WriteString("  <key>ProcessType</key><string>Background</string>\n")
		b.WriteString("  <key>LowPriorityIO</key><true/>\n")
		b.WriteString("  <key>Nice</key><integer>5</integer>\n")
	}
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlText(j.Log))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlText(j.ErrLog))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// xmlText escapes s for a plist <string>: a path holding & or < otherwise makes
// the whole job unloadable.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// InstalledArgs returns the arguments the installed job starts with, or nil
// when there is no job. The plist is what Install wrote and what launchd
// loads at login, so it is the job's own record of how it runs.
func InstalledArgs(label string) []string {
	p, err := plistPath(label)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return argsIn(b)
}

// argsIn returns a plist's ProgramArguments, or nil when it has none.
func argsIn(plist []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	var key string
	var args []string
	inArgs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				if dec.DecodeElement(&key, &t) != nil {
					return nil
				}
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var v string
				if dec.DecodeElement(&v, &t) != nil {
					return nil
				}
				if inArgs {
					args = append(args, v)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inArgs = false
			}
		}
	}
	return args
}

// Install writes the job's plist and starts it, replacing any running copy,
// and returns the plist's path.
func (j Job) Install() (string, error) {
	p, err := plistPath(j.Label)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(j.plist()), 0o600); err != nil {
		return "", err
	}
	// bootout then bootstrap restarts a running job on the new plist; no
	// kickstart is needed.
	Stop(j.Label)

	var lastOut []byte
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		out, err := exec.CommandContext(context.Background(), launchctl, "bootstrap", "gui/"+uid(), p).CombinedOutput()
		if err == nil {
			break
		}
		lastOut, lastErr = out, err
		time.Sleep(500 * time.Millisecond)
	}

	// Verified, not assumed: a failed bootstrap leaves no job at all, since
	// the previous one is already gone.
	if !Loaded(j.Label) {
		if lastErr != nil {
			return p, fmt.Errorf("launchctl bootstrap: %w: %s", lastErr, bytes.TrimSpace(lastOut))
		}
		return p, fmt.Errorf("launchctl reported success but %s is not loaded, so it is not running", j.Label)
	}
	return p, nil
}

// Stop unloads the job, if launchd holds it, and waits until it has let go.
//
// bootout takes a service target (gui/<uid>/<label>), not the plist path:
// given the path it fails quietly, a bootstrap then reports success for the
// still-loaded job, and the previous binary keeps running. And it returns
// before the job has exited: bootstrapping a label launchd still holds fails
// with a generic "Input/output error", leaving the old job gone and the new
// one refused.
func Stop(label string) {
	_ = exec.CommandContext(context.Background(), launchctl, "bootout", target(label)).Run()
	for i := 0; i < 100 && Loaded(label); i++ {
		time.Sleep(100 * time.Millisecond)
	}
}

// Running reports whether launchd currently has the job alive, along with the
// program path it was told to start. Not whether the plist exists: a job whose
// binary was deleted has the same plist as a healthy one, while launchd
// retries it on a ten-second throttle and nothing runs.
func Running(label string) (running bool, program string, err error) {
	out, err := exec.CommandContext(context.Background(),
		launchctl, "print", target(label)).Output()
	if err != nil {
		return false, "", err
	}
	text := string(out)
	for _, line := range strings.Split(text, "\n") {
		f := strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(f, "program = "); ok && program == "" {
			program = strings.TrimSpace(p)
		}
		if f == "state = running" {
			running = true
		}
	}
	return running, program, nil
}

// Loaded reports whether launchd currently holds the job.
func Loaded(label string) bool {
	return exec.CommandContext(context.Background(), launchctl, "print", target(label)).Run() == nil
}

const launchctl = "/bin/launchctl"

// target is the launchd service target for this user's copy of the job.
func target(label string) string { return "gui/" + uid() + "/" + label }

// Uninstall stops the job and deletes its plist, returning the plist's path.
func Uninstall(label string) (string, error) {
	p, err := plistPath(label)
	if err != nil {
		return "", err
	}
	Stop(label)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, nil
}

func uid() string { return fmt.Sprint(os.Getuid()) }

// InstallBinary copies the executable at src to dst, the stable path a job
// starts. Not the path install ran from: a checkout's bin/ is deleted by
// `make clean` or invalidated by a moved checkout, and launchd then retries the
// missing binary forever. And checkouts tend to live under ~/Documents, which
// macOS may not let a LaunchAgent read at all.
func InstallBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Staged then renamed, so a failure part-way through cannot leave a
	// half-written binary where launchd will try to start one, and a reinstall
	// from the install location reads the old file while writing the new. The
	// staged file is removed on every path, its errors dropped: after the
	// rename there is none, and launchd never starts the temp name.
	tmp := dst + ".new"
	defer func() { _ = os.Remove(tmp) }()
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700) //nolint:gosec
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
