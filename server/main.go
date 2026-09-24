// Command llm-tracker-server ingests agent batches and serves the dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/samiashi/llm-tracker/server/internal/api"
	"github.com/samiashi/llm-tracker/server/internal/auth"
	"github.com/samiashi/llm-tracker/server/internal/db"
	"github.com/samiashi/llm-tracker/server/internal/web"
)

var version = "dev"

// main exits with run's result rather than inside it: os.Exit skips deferred
// functions, so every exit path in run still closes the database.
func main() { os.Exit(run()) }

func run() int {
	addr := flag.String("addr", "127.0.0.1:8790", "listen address")
	dsn := flag.String("db", "llm-tracker.db", "sqlite path")
	retainDays := flag.Int("retain", 0,
		"delete raw events older than N days, rolling each day up first; 0 keeps everything")
	revoke := flag.String("revoke", "",
		"revoke every ingest token enrolled by this GitHub login and release its machines, then exit")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	database, err := db.Open(*dsn)
	if err != nil {
		log.Error("open database", "err", err)
		return 1
	}
	defer database.Close()
	log.Info("price table", "version", database.PriceTableVersion())

	if *revoke != "" {
		r, err := database.RevokeTokens(context.Background(), *revoke)
		if err != nil {
			log.Error("revoke", "err", err)
			return 1
		}
		if r == (db.Revoked{}) {
			log.Warn("nothing to revoke", "login", *revoke, "note", "logins are GitHub usernames")
			return 0
		}
		log.Info("revoked", "login", *revoke, "tokens", r.Tokens,
			"machines_released", r.Machines, "accounts_released", r.Accounts)
		return 0
	}

	if err := loopbackOnly(*addr); err != nil {
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
	database.Pruning = *retainDays > 0
	go maintain(bgCtx, database, *retainDays, log)
	if *retainDays == 0 {
		if day, err := database.RollupsBefore(context.Background()); err == nil && day != "" {
			log.Info("retention is off: agents may now deliver what pruning refused, "+
				"but days already rolled up stay rolled up", "rolled_up_before", day)
		}
	}

	httpSrv := &http.Server{
		Addr:    *addr,
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
		log.Info("listening", "addr", *addr, "version", version, "db", *dsn)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
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
