// Command llm-tracker-agent collects local AI coding-agent token usage and
// forwards counts -- never content -- to the tracker's server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/launchd"
	"github.com/samiashi/llm-tracker/agent/internal/store"
)

var version = "dev"

// defaultServer is the tracker enroll joins: the server's own default
// address, on this machine.
const defaultServer = "http://127.0.0.1:8790"

// ghCommand runs the GitHub CLI against github.com: gh takes the host from
// GH_HOST, and anything that set it would choose the server a token comes
// from.
func ghCommand(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "gh", args...)
	c.Env = append(os.Environ(), "GH_HOST=github.com")
	return c
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `llm-tracker-agent -- token-usage collector

commands:
  scan        run one collection pass and print what was found
  sync        upload pending events to the server
  run         collect and sync on an interval (used by launchd)
  status      show local archive, accounts and configuration
  enroll      join the tracker as your GitHub login
  sources     list every registered harness adapter
  probe       dry-run one adapter and report what it would extract
  rewind      re-read one source, upgrading rows in place (after adding a field)
  resend      re-upload the whole local archive to the server
  resync      re-read one source from scratch, discarding its rows first
  install     install and start the LaunchAgent
  uninstall   stop and remove the LaunchAgent
  version     print version

scan, sync, rewind, resend and resync need the collector stopped; run while it
is running, they say how to stop it and start it again.

flags:
  -data <dir>       state directory (default: the installed collector's,
                    else ~/.llm-tracker)
  -server <url>     enroll: the tracker's address, if not %s
  -source <name>    rewind / resync: any registered source (see: sources)
  -yes              resync: skip the confirmation prompt
  -v                verbose logging
`, defaultServer)
}

func run() error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	dataDir := fs.String("data", "", "state directory")
	server := fs.String("server", defaultServer, "tracker to enrol with")
	source := fs.String("source", "", "source name for resync")
	yes := fs.Bool("yes", false, "skip the confirmation prompt on resync")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Usage = usage

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return nil
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	*dataDir = collectorDataDir(*dataDir, home)
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}

	// The daemon logs at Info so its log shows it working. One-shot commands
	// print their results to stdout, and logging them too is noise.
	level := slog.LevelWarn
	if cmd == "run" {
		level = slog.LevelInfo
	}
	if *verbose {
		level = slog.LevelDebug
	}

	// The daemon logs to its own capped file (see config.RotatingFile), the
	// agent.log that `install` names, not to launchd's uncapped redirect.
	var out io.Writer = os.Stderr
	if cmd == "run" {
		lf, lerr := config.OpenLog(config.LogPath(*dataDir), config.DefaultLogMaxBytes)
		if lerr != nil {
			return lerr
		}
		defer lf.Close()
		out = lf
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "scan":
		return cmdScan(*dataDir, home, log)
	case "sync":
		return cmdSync(*dataDir, log)
	case "run":
		return cmdRun(*dataDir, home, log)
	case "status":
		return cmdStatus(*dataDir)
	case "enroll":
		return cmdEnroll(*dataDir, *server, log)
	case "sources":
		return cmdSources(home)
	case "probe":
		return cmdProbe(home, fs.Arg(0))
	case "resend":
		return cmdResend(*dataDir)
	case "rewind":
		return cmdRewind(*dataDir, home, *source)
	case "resync":
		return cmdResync(*dataDir, home, *source, *yes)
	case "install":
		return cmdInstall(*dataDir)
	case "uninstall":
		return cmdUninstall()
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func openStore(dataDir string) (*store.Store, error) {
	return store.Open(store.Path(dataDir))
}

// defaultDataDir is where state lives without -data.
func defaultDataDir(home string) string { return filepath.Join(home, ".llm-tracker") }

// agentCmd renders a command to suggest, naming -data only when it is not the
// default: a suggestion without it would act on a different directory.
func agentCmd(dataDir string, args ...string) string {
	parts := append([]string{"llm-tracker-agent"}, args...)
	if home, err := os.UserHomeDir(); err != nil || dataDir != defaultDataDir(home) {
		parts = append(parts, "-data", shellQuote(dataDir))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			!strings.ContainsRune("/._-+:@%=,", r)
	}) < 0
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// openStoreLocked opens the store behind the one-agent-per-directory lock, for
// every command that writes. Read-only commands (`status`, `probe`) skip it,
// so they work while the daemon runs.
func openStoreLocked(dataDir string) (*store.Store, func(), error) {
	lock, err := store.Acquire(dataDir)
	if err != nil {
		return nil, nil, lockedHint(err, dataDir)
	}
	st, err := openStore(dataDir)
	if err != nil {
		_ = lock.Release()
		return nil, nil, err
	}
	return st, func() {
		_ = st.Close()
		_ = lock.Release()
	}, nil
}

// lockedHint adds the way past a held lock: how to stop the collector and how
// to start it again, or collection stays off afterwards.
func lockedHint(err error, dataDir string) error {
	if !errors.Is(err, store.ErrLocked) {
		return err
	}
	return fmt.Errorf("%w -- normally the background collector.\n"+
		"Stop it, run this command again, then restart it:\n"+
		"  launchctl bootout gui/$(id -u)/%s\n"+
		"  %s", err, launchd.Label, agentCmd(dataDir, "install"))
}

func humanTokens(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// collectorDataDir is the directory a command acts on: -data if given, else
// the one the installed collector's LaunchAgent names, else the default. A
// collector installed with -data would otherwise be enrolled, queried and
// reinstalled against a store it never reads.
func collectorDataDir(dataFlag, home string) string {
	if dataFlag != "" {
		return dataFlag
	}
	if d := launchd.InstalledDataDir(); d != "" {
		return d
	}
	return defaultDataDir(home)
}
