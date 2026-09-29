package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/agent/internal/tracker"
	"github.com/samiashi/llm-tracker/launchd"
)

func cmdStatus(dataDir string) error {
	st, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	cfg, _ := config.Load(dataDir)
	id, err := identity.MachineID(ctx, st)
	if err != nil {
		return err
	}
	fmt.Println("version: ", version)
	fmt.Println("machine: ", id)
	fmt.Println("state:   ", store.Path(dataDir))
	if cfg.ServerURL == "" {
		fmt.Println("server:   (not enrolled -- collecting locally only)")
	} else {
		fmt.Println("server:  ", cfg.ServerURL)
	}

	// The verdict first: it is the only line most people want. From launchd,
	// not the plist (see launchd.Running).
	fmt.Println()
	running, program, lerr := launchd.Running(collectorLabel)
	for _, line := range daemonVerdict(dataDir, running, program, lerr) {
		fmt.Println(line)
	}

	h, err := st.Health(ctx)
	if err != nil {
		return err
	}
	fmt.Println("last pass:", since(h.LastPassAt),
		fmt.Sprintf("(found %d, stored %d)", h.LastFound, h.LastStored))
	switch {
	case h.LastSyncErr != "":
		fmt.Println("last sync: FAILING --", h.LastSyncErr)
		if tracker.AuthRejected(h.LastSyncErr) {
			fmt.Println("          the token was rejected; nothing will upload until it is replaced:")
			fmt.Println("          " + agentCmd(dataDir, "enroll"))
		}
	case h.LastSyncAt.IsZero():
		fmt.Println("last sync: never")
	default:
		fmt.Println("last sync:", since(h.LastSyncAt),
			fmt.Sprintf("(%d events)", h.LastUploaded))
	}

	fmt.Println("\naccounts detected:")
	accts := identity.All()
	if len(accts) == 0 {
		fmt.Println("  (none)")
	}
	for _, a := range accts {
		fmt.Printf("  %-10s %-28s plan=%s\n", a.Provider, a.Email, a.PlanType)
	}

	events, unsent, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\narchive: %d events, %d awaiting upload, %s on disk\n",
		events, unsent, humanBytes(archiveBytes(dataDir)))
	if refused, err := st.CountRefused(ctx); err == nil && refused > 0 {
		fmt.Printf("         %d predate the server's retention window and will not be uploaded\n", refused)
	}
	return nil
}

// daemonVerdict is status's word on the collector, from what launchd reports,
// with the command that fixes it for this data directory: a bare install
// would start a collector on the default one instead.
func daemonVerdict(dataDir string, running bool, program string, lerr error) []string {
	install := agentCmd(dataDir, "install")
	var out []string
	switch {
	case lerr != nil:
		out = append(out, "daemon:   not installed (run: "+install+")")
	case !running:
		out = append(out, "daemon:   installed but NOT running",
			"          launchctl print gui/$(id -u)/"+collectorLabel+"  # for why")
	default:
		out = append(out, "daemon:   running")
	}
	if program != "" {
		if _, err := os.Stat(program); err != nil {
			out = append(out, "          WARNING: its binary is missing: "+program,
				"          launchd cannot start it. Re-run: "+install)
		}
	}
	return out
}

// archiveBytes totals the store and its -wal and -shm files, since the -wal
// alone can be the larger half of what the directory costs.
func archiveBytes(dataDir string) int64 {
	var total int64
	base := store.Path(dataDir)
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// since renders how long ago t was, or "never" for the zero time.
func since(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
