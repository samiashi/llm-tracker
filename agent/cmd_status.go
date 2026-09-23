package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/launchd"
	"github.com/samiashi/llm-tracker/agent/internal/sync"
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
	fmt.Println("state:   ", filepath.Join(dataDir, "agent.db"))
	if cfg.ServerURL == "" {
		fmt.Println("server:   (not enrolled -- collecting locally only)")
	} else {
		fmt.Println("server:  ", cfg.ServerURL)
	}

	// The verdict first: it is the only line most people want. From launchd,
	// not the plist (see launchd.Running).
	fmt.Println()
	running, program, lerr := launchd.Running()
	switch {
	case lerr != nil:
		fmt.Println("daemon:   not installed (run: llm-tracker-agent install)")
	case !running:
		fmt.Println("daemon:   installed but NOT running")
		fmt.Println("          launchctl print gui/$(id -u)/" + launchd.Label + "  # for why")
	default:
		fmt.Println("daemon:   running")
	}
	if program != "" {
		if _, serr := os.Stat(program); serr != nil {
			fmt.Println("          WARNING: its binary is missing:", program)
			fmt.Println("          launchd cannot start it. Re-run: llm-tracker-agent install")
		}
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
		if sync.AuthRejected(h.LastSyncErr) {
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

// archiveBytes totals the store and its -wal and -shm files, since the -wal
// alone can be the larger half of what the directory costs.
func archiveBytes(dataDir string) int64 {
	var total int64
	base := filepath.Join(dataDir, "agent.db")
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
