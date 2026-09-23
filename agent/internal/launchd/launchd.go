// Package launchd installs the agent as a per-user LaunchAgent.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/config"
)

const Label = "io.github.samiashi.llm-tracker"

// plistPath is the per-user agent location. A LaunchAgent rather than a
// LaunchDaemon: this collects one person's usage and has no business running
// as root or touching other accounts on the machine.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// plist renders the job definition. LowPriorityIO and a positive Nice keep a
// pass that reads tens of gigabytes from making the machine feel slow: an agent
// people notice is an agent people uninstall.
func plist(binPath, dataDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>-data</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>Nice</key><integer>5</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, Label, xmlText(binPath), xmlText(dataDir),
		xmlText(config.LogPath(dataDir)),
		xmlText(filepath.Join(dataDir, "agent.err.log")))
}

// xmlText escapes s for a plist <string>: a path holding & or < otherwise makes
// the whole job unloadable.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// InstalledDataDir returns the data directory the installed job runs on, or ""
// when there is no job or it names none. The plist is what Install wrote and
// what launchd loads at login, so it is the collector's own record.
func InstalledDataDir() string {
	p, err := plistPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return dataDirIn(b)
}

// dataDirIn returns the argument after -data in a plist's ProgramArguments.
func dataDirIn(plist []byte) string {
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
					return ""
				}
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var v string
				if dec.DecodeElement(&v, &t) != nil {
					return ""
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
	for i, a := range args {
		if a == "-data" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func Install(binPath, dataDir string) (string, error) {
	p, err := plistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(plist(binPath, dataDir)), 0o600); err != nil {
		return "", err
	}
	// Replace any previous registration. bootout takes a service target
	// (gui/<uid>/<label>), not the plist path: given the path it fails
	// quietly, bootstrap then reports success for the still-loaded job, and
	// the previous binary keeps running. bootout then bootstrap restarts the
	// job on its own; no kickstart is needed.
	_ = exec.CommandContext(context.Background(), launchctl, "bootout", target()).Run()

	// bootout returns before the job has exited, and bootstrapping a label
	// launchd still holds fails with a generic "Input/output error", leaving
	// the old job gone and the new one refused. So wait for the label to go,
	// then retry once.
	for i := 0; i < 20 && Loaded(); i++ {
		time.Sleep(100 * time.Millisecond)
	}

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
	if !Loaded() {
		if lastErr != nil {
			return p, fmt.Errorf("launchctl bootstrap: %w: %s", lastErr, bytes.TrimSpace(lastOut))
		}
		return p, fmt.Errorf("launchctl reported success but %s is not loaded; "+
			"the collector is not running", Label)
	}
	return p, nil
}

// Running reports whether launchd currently has the job alive, along with the
// program path it was told to start. Not whether the plist exists: a job whose
// binary was deleted has the same plist as a healthy one, while launchd
// retries it on a ten-second throttle and nothing collects.
func Running() (running bool, program string, err error) {
	out, err := exec.CommandContext(context.Background(),
		launchctl, "print", target()).Output()
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
func Loaded() bool {
	return exec.CommandContext(context.Background(), launchctl, "print", target()).Run() == nil
}

const launchctl = "/bin/launchctl"

// target is the launchd service target for this user's copy of the job.
func target() string { return "gui/" + uid() + "/" + Label }

func Uninstall() (string, error) {
	p, err := plistPath()
	if err != nil {
		return "", err
	}
	_ = exec.CommandContext(context.Background(), launchctl, "bootout", target()).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, nil
}

func uid() string { return fmt.Sprint(os.Getuid()) }
