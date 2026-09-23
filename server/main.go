// Command llm-tracker-server ingests agent batches and serves the dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

//nolint:gocyclo // a flat startup sequence reads better than split helpers
func run() int {
	addr := flag.String("addr", "127.0.0.1:8790", "listen address")
	dsn := flag.String("db", "llm-tracker.db", "sqlite path")
	pruneDays := flag.Int("prune", 0, "roll up and delete raw events older than N days, then exit")
	retainDays := flag.Int("retain", 0,
		"delete raw events older than N days, rolling each day up first; 0 keeps everything")
	healthcheck := flag.Bool("healthcheck", false, "probe a running server on this host, then exit")
	revoke := flag.String("revoke", "", "revoke every ingest token enrolled by this GitHub login, then exit")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	// Before anything is opened: a probe must not touch the database the
	// running server has open.
	if *healthcheck {
		return probe(*addr)
	}

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

	if *pruneDays > 0 {
		res, err := database.Prune(context.Background(), pruneCutoff(*pruneDays))
		if err != nil {
			log.Error("prune", "err", err)
			return 1
		}
		logPruned(log, res)
		return 0
	}

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

	// GitHub auth is what gates the dashboard and what every agent enrols
	// through, so there is no mode without it.
	cfg, err := authConfig()
	if err != nil {
		log.Error("refusing to start", "err", err)
		return 1
	}
	authn, err := auth.New(cfg)
	if err != nil {
		log.Error("refusing to start", "err", err)
		return 1
	}
	authn.Log = log
	srv := &api.Server{DB: database, Log: log, Version: version, Enroll: authn}

	// Cancelled on shutdown, so a scheduled prune in flight stops rather than
	// writing into a database the process is about to close.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	// Beside live ingest rather than before serving: every write reprice makes
	// is conditional, and ingest prices what it stores meanwhile.
	go repriceIfChanged(bgCtx, database, log)
	// The floor ingest enforces follows the same switch: a server that keeps
	// everything accepts everything, including the backlog its agents have
	// been holding since it last pruned.
	database.Pruning = *retainDays > 0
	if *retainDays > 0 {
		go pruneDaily(bgCtx, database, *retainDays, log)
	} else if day, err := database.RollupsBefore(context.Background()); err == nil && day != "" {
		log.Info("retention is off: agents may now deliver what pruning refused, "+
			"but days already rolled up stay rolled up", "rolled_up_before", day)
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: newHandler(srv, authn, web.Handler()),
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

// authConfig reads GitHub auth's settings from the environment, naming every
// one that is missing rather than the first.
func authConfig() (auth.Config, error) {
	var missing []string
	need := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := auth.Config{
		ClientID:     need("LLM_TRACKER_GITHUB_CLIENT_ID"),
		ClientSecret: need("LLM_TRACKER_GITHUB_CLIENT_SECRET"),
		Org:          need("LLM_TRACKER_GITHUB_ORG"),
		BaseURL:      need("LLM_TRACKER_BASE_URL"),
		SessionKey:   []byte(need("LLM_TRACKER_SESSION_KEY")),
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("set %s (.env.example says what each is)", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// newHandler assembles the request chain. The security headers wrap
// everything, so /auth and the middleware's own refusals carry them too.
func newHandler(srv *api.Server, authn *auth.Authenticator, spa http.Handler) http.Handler {
	mux := http.NewServeMux()
	authn.Routes(mux)
	mux.Handle("/", srv.Routes(spa))
	return api.WithSecurityHeaders(api.WithGzip(authn.Middleware(mux)))
}

// probe asks a server already running in this container whether it is serving.
// The binary checks itself because the distroless runtime image has no shell,
// curl or wget for a HEALTHCHECK to run.
//
// It dials loopback on the listen port: a server bound to 0.0.0.0 answers on
// 127.0.0.1 inside its own container, and dialling 0.0.0.0 is not portable.
func probe(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: cannot read a port from", addr)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
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
	run := func() {
		cutoff := pruneCutoff(retain)
		res, err := d.Prune(ctx, cutoff)
		if err != nil {
			// Logged, not fatal: the dashboard is still serving, and a prune
			// that fails tonight can succeed tomorrow.
			log.Error("scheduled prune failed", "cutoff", cutoff, "err", err)
			return
		}
		if res.DaysRolled > 0 {
			logPruned(log, res)
		}
		if res.VacuumErr != nil {
			log.Warn("pruned, but returning the freed space to the disk failed; the next prune retries",
				"err", res.VacuumErr)
		}
	}

	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// pruneCutoff is the first UTC day a prune keeping the last days days leaves raw.
func pruneCutoff(days int) string {
	return time.Now().UTC().AddDate(0, 0, -days).Format(time.DateOnly)
}

// logPruned reports a prune the same way from -prune and from -retain.
func logPruned(log *slog.Logger, res *db.PruneResult) {
	log.Info("pruned", "cutoff", res.Cutoff, "days", res.DaysRolled,
		"rollup_rows", res.RollupRows, "events_pruned", res.EventsPruned,
		"machines_pruned", res.MachinesPruned, "vacuumed", res.Vacuumed)
}
