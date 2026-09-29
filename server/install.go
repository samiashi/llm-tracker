package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/samiashi/llm-tracker/launchd"
)

// serverLabel names the server's LaunchAgent.
const serverLabel = "io.github.samiashi.llm-tracker.server"

// serverDir holds the installed server's binary, database and log: outside
// the checkout, for the reason the binary is copied out of it
// (launchd.InstallBinary), and outside the collector's ~/.llm-tracker, which
// the README says to rm -rf to remove the collector.
func serverDir(home string) string { return filepath.Join(home, ".llm-tracker-server") }

// defaultDB is the installed server's database, and so every command's: a
// server started by hand on another database would take the collector's
// uploads on the same port, and they would never reach the installed one.
func defaultDB(home string) string { return filepath.Join(serverDir(home), "llm-tracker.db") }

func serverLog(home string) string { return filepath.Join(serverDir(home), "server.log") }

// serverJob is the server's LaunchAgent. Not at background priority: it
// answers the dashboard while someone is looking at it.
func serverJob(bin, home string, retain int) launchd.Job {
	args := []string{bin, "-db", defaultDB(home)}
	if retain > 0 {
		args = append(args, "-retain", strconv.Itoa(retain))
	}
	return launchd.Job{Label: serverLabel, Args: args, Log: serverLog(home), ErrLog: serverLog(home)}
}

func cmdInstall(home string, retain int) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	bin := filepath.Join(serverDir(home), "llm-tracker-server")
	if err := launchd.InstallBinary(src, bin); err != nil {
		return err
	}

	// Stopped first, so the port is free unless something else holds it: a
	// server started by hand would keep answering, while the job failed to
	// bind at every restart.
	launchd.Stop(serverLabel)
	if err := portFree(defaultAddr); err != nil {
		return fmt.Errorf("%s is taken, by a server started by hand or another program: "+
			"stop it and install again (%w)", defaultAddr, err)
	}
	p, err := serverJob(bin, home, retain).Install()
	if err != nil {
		return err
	}
	if err := waitServing(defaultAddr, time.Minute); err != nil {
		return fmt.Errorf("the job is loaded but not serving: %w\nsee %s", err, serverLog(home))
	}
	fmt.Println("installed and started:", p)
	fmt.Println("dashboard: http://" + defaultAddr)
	fmt.Println("database:", defaultDB(home))
	fmt.Println("logs:", serverLog(home))
	return nil
}

// portFree reports why addr cannot be listened on, if it cannot.
func portFree(addr string) error {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	return l.Close()
}

// waitServing polls the health check until it answers or the timeout passes:
// a loaded job is not a serving one, and a server that cannot open its
// database exits and is retried every ten seconds.
func waitServing(addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("the health check returned %s", resp.Status)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func cmdUninstall(home string) error {
	p, err := launchd.Uninstall(serverLabel)
	if err != nil {
		return err
	}
	fmt.Println("removed:", p)
	fmt.Println("the database stays in", serverDir(home))
	return nil
}
