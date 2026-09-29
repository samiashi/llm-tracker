package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/launchd"
)

// collectorLabel names the collector's LaunchAgent.
const collectorLabel = "io.github.samiashi.llm-tracker"

// collectorJob is the collector's LaunchAgent, at background priority: a pass
// that reads tens of gigabytes must not make the machine feel slow, and an
// agent people notice is an agent people uninstall.
func collectorJob(bin, dataDir string) launchd.Job {
	return launchd.Job{
		Label:      collectorLabel,
		Args:       []string{bin, "run", "-data", dataDir},
		Log:        config.LogPath(dataDir),
		ErrLog:     filepath.Join(dataDir, "agent.err.log"),
		Background: true,
	}
}

func cmdInstall(dataDir string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	bin := filepath.Join(dataDir, "bin", "llm-tracker-agent")
	if err := launchd.InstallBinary(src, bin); err != nil {
		return err
	}

	p, err := collectorJob(bin, dataDir).Install()
	if err != nil {
		return err
	}
	fmt.Println("installed and started:", p)
	fmt.Println("binary:", bin)
	fmt.Println("logs:", config.LogPath(dataDir))
	return nil
}

// installedDataDir returns the data directory the installed collector runs
// on, or "" when there is no job or it names none.
func installedDataDir() string {
	args := launchd.InstalledArgs(collectorLabel)
	for i, a := range args {
		if a == "-data" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func cmdUninstall() error {
	p, err := launchd.Uninstall(collectorLabel)
	if err != nil {
		return err
	}
	fmt.Println("removed:", p)
	return nil
}
