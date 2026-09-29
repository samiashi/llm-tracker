// Command llm-tracker-server ingests agent batches and serves the dashboard.
// `install` runs it as a LaunchAgent, from its own copy in ~/.llm-tracker-server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samiashi/llm-tracker/server/internal/api"
	"github.com/samiashi/llm-tracker/server/internal/auth"
	"github.com/samiashi/llm-tracker/server/internal/db"
	"github.com/samiashi/llm-tracker/server/internal/web"
)

var version = "dev"

// defaultAddr is where the dashboard is, and where the collector uploads.
const defaultAddr = "127.0.0.1:8790"

// main exits with run's result rather than inside it: os.Exit skips deferred
// functions, so every exit path still closes the database.
func main() { os.Exit(run(os.Args[1:])) }

func usage() {
	fmt.Fprintf(os.Stderr, `llm-tracker-server -- ingest API and dashboard, at http://%s

commands:
  (none)      serve in the foreground
  install     install and start the LaunchAgent: serve now and at every login
  uninstall   stop and remove the LaunchAgent; the database stays

flags:
  -db <path>         database (default: the installed server's,
                     ~/.llm-tracker-server/llm-tracker.db)
  -retain <days>     keep this many days of raw events, rolling each older day
                     up first; 0, the default, keeps everything. install takes
                     it too, and the job keeps it
  -revoke <login>    revoke every ingest token this GitHub login enrolled and
                     release its machines, then exit
  -addr <host:port>  listen address, loopback only (default %s)
  -v                 verbose logging
`, defaultAddr, defaultAddr)
}

// options are the flags a command runs with.
type options struct {
	addr, db, revoke string
	retain           int
	verbose          bool
}

// parseArgs splits a command line into its command, "" to serve, and its
// options. install takes -retain alone and uninstall nothing: the job serves
// on the default address and database, so any other flag would be dropped
// without a word.
func parseArgs(args []string, home string) (string, options, error) {
	var o options
	fs := flag.NewFlagSet("llm-tracker-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.addr, "addr", defaultAddr, "")
	fs.StringVar(&o.db, "db", defaultDB(home), "")
	fs.IntVar(&o.retain, "retain", 0, "")
	fs.StringVar(&o.revoke, "revoke", "", "")
	fs.BoolVar(&o.verbose, "v", false, "")

	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", o, err
	}
	if fs.NArg() > 0 {
		return "", o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	switch cmd {
	case "":
		return cmd, o, nil
	case "install", "uninstall":
		var err error
		fs.Visit(func(f *flag.Flag) {
			if err == nil && (cmd == "uninstall" || f.Name != "retain") {
				err = fmt.Errorf("%s does not take -%s", cmd, f.Name)
			}
		})
		return cmd, o, err
	}
	return "", o, fmt.Errorf("unknown command %q", cmd)
}

func run(args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cmd, o, err := parseArgs(args, home)
	if err != nil {
		usage()
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		return 2
	}
	switch cmd {
	case "install":
		err = cmdInstall(home, o.retain)
	case "uninstall":
		err = cmdUninstall(home)
	default:
		return serve(o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func serve(o options) int {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// The default database's directory does not exist before the first run.
	if err := os.MkdirAll(filepath.Dir(o.db), 0o700); err != nil {
		log.Error("open database", "err", err)
		return 1
	}
	database, err := db.Open(o.db)
	if err != nil {
		log.Error("open database", "err", err)
		return 1
	}
	defer database.Close()
	log.Info("price table", "version", database.PriceTableVersion())

	if o.revoke != "" {
		r, err := database.RevokeTokens(context.Background(), o.revoke)
		if err != nil {
			log.Error("revoke", "err", err)
			return 1
		}
		if r == (db.Revoked{}) {
			log.Warn("nothing to revoke", "login", o.revoke, "note", "logins are GitHub usernames")
			return 0
		}
		log.Info("revoked", "login", o.revoke, "tokens", r.Tokens,
			"machines_released", r.Machines, "accounts_released", r.Accounts)
		return 0
	}

	if err := loopbackOnly(o.addr); err != nil {
		log.Error("refusing to start", "err", err)
		return 1
	}
	g := auth.NewLocal()
	srv := &api.Server{DB: database, Log: log, Version: version, Enroll: g}

	// Cancelled on shutdown, so a scheduled prune in flight stops rather than
	// writing into a database the process is about to close.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	// The floor ingest enforces follows the same switch: a server that keeps
	// everything accepts everything, including the backlog its agents have
	// been holding since it last pruned.
	database.Pruning = o.retain > 0
	go maintain(bgCtx, database, o.retain, log)
	if o.retain == 0 {
		if day, err := database.RollupsBefore(context.Background()); err == nil && day != "" {
			log.Info("retention is off: agents may now deliver what pruning refused, "+
				"but days already rolled up stay rolled up", "rolled_up_before", day)
		}
	}

	httpSrv := &http.Server{
		Addr:    o.addr,
		Handler: newHandler(srv, g, web.Handler()),
		// Every phase is bounded, or a client dripping its body or holding a
		// keep-alive open keeps a goroutine and a socket as long as it likes.
		// Read and write are generous: a first-run backfill batch and a full
		// CSV export are both legitimately slow.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", o.addr, "version", version, "db", o.db)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			if errors.Is(err, syscall.EADDRINUSE) {
				log.Error("another server holds the address; the installed one stops with: llm-tracker-server uninstall")
			}
			serveErr <- err
		}
		close(serveErr)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case err := <-serveErr:
		if err != nil {
			return 1
		}
	}

	stopBackground()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		// Handlers still running are cut off when the process exits, and any
		// transaction they hold rolls back.
		log.Warn("shutdown timed out with requests in flight", "err", err)
	}
	log.Info("stopped")
	return 0
}

// loopbackOnly refuses a listen address another machine could reach: the
// dashboard has no sign-in, so the address is what keeps it to this machine.
func loopbackOnly(addr string) error {
	if !auth.IsLoopback(addr) {
		return fmt.Errorf("%s can be reached from other machines, and the dashboard has no "+
			"sign-in: listen on 127.0.0.1", addr)
	}
	return nil
}

// newHandler assembles the request chain. The security headers wrap
// everything, so the gate's own refusals carry them too.
func newHandler(srv *api.Server, g *auth.Local, spa http.Handler) http.Handler {
	return api.WithSecurityHeaders(api.WithGzip(g.Middleware(srv.Routes(spa))))
}

// maintain reprices stored events for this build, then, when retain is set,
// prunes daily. Reprice runs beside live ingest rather than before serving:
// every write it makes is conditional, and ingest prices what it stores
// meanwhile. The prune waits for it, because a rollup freezes the costs it
// sums, and started beside it would freeze the last build's.
func maintain(ctx context.Context, d *db.DB, retain int, log *slog.Logger) {
	repriceIfChanged(ctx, d, log)
	if retain > 0 {
		pruneDaily(ctx, d, retain, log)
	}
}

// repriceIfChanged brings stored costs up to this build's prices, once per
// build (see db.RepriceIfChanged).
func repriceIfChanged(ctx context.Context, d *db.DB, log *slog.Logger) {
	n, ran, err := d.RepriceIfChanged(ctx, version)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			log.Error("repricing for this build failed; the next start retries", "err", err)
		}
	case ran:
		log.Info("repriced stored events for this build", "changed", n,
			"price_table", d.PriceTableVersion())
		if day, err := d.RollupsBefore(ctx); err == nil && day != "" {
			log.Warn("rolled-up days keep their original prices", "before", day,
				"why", "a rollup stores sums, not the per-event dimensions pricing needs")
		}
	}
}

// pruneDaily rolls up and deletes raw events older than retain days, once at
// startup and then every 24 hours, until ctx is cancelled.
func pruneDaily(ctx context.Context, d *db.DB, retain int, log *slog.Logger) {
	pruneOnce := func() {
		cutoff := pruneCutoff(retain)
		res, err := d.Prune(ctx, cutoff)
		if err != nil {
			// Logged, not fatal: the dashboard is still serving, and a prune
			// that fails tonight can succeed tomorrow.
			log.Error("scheduled prune failed", "cutoff", cutoff, "err", err)
			return
		}
		if res.DaysRolled > 0 {
			log.Info("pruned", "cutoff", res.Cutoff, "days", res.DaysRolled,
				"rollup_rows", res.RollupRows, "events_pruned", res.EventsPruned,
				"machines_pruned", res.MachinesPruned, "vacuumed", res.Vacuumed)
		}
		if res.VacuumErr != nil {
			log.Warn("pruned, but returning the freed space to the disk failed; the next prune retries",
				"err", res.VacuumErr)
		}
	}

	pruneOnce()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pruneOnce()
		}
	}
}

// pruneCutoff is the first UTC day left raw by a prune that keeps the last n
// days.
func pruneCutoff(n int) string {
	return time.Now().UTC().AddDate(0, 0, -n).Format(time.DateOnly)
}
