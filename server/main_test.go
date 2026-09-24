package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/api"
	"github.com/samiashi/llm-tracker/server/internal/auth"
	"github.com/samiashi/llm-tracker/server/internal/db"
	"github.com/samiashi/llm-tracker/server/internal/web"
)

// token is an ingest token handler enrolled.
var token string

// handler is the server as main assembles it, with a small built dashboard
// in place of the embedded one.
func handler(t *testing.T) http.Handler {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if token, err = d.IssueToken(context.Background(), "tester", "test"); err != nil {
		t.Fatal(err)
	}

	g := auth.NewLocal()
	srv := &api.Server{DB: d, Log: slog.New(slog.DiscardHandler), Version: "v1.0.0", Enroll: g}
	return newHandler(srv, g, web.NewHandler(fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>dashboard</title>")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}))
}

// reply is what a client received: the status and the header as sent.
type reply struct {
	StatusCode int
	Header     http.Header
}

// send serves one request, addressed to host, carrying what an agent's
// upload carries.
func send(h http.Handler, host, method, target string) reply {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"v":1,"machine_id":"m","events":[]}`)
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	return reply{res.StatusCode, res.Header}
}

// The gate's refusals included: a page that rebinds its hostname is still a
// page, and gets no frame and no cache.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := handler(t)
	for _, tc := range []struct{ host, method, path string }{
		{"127.0.0.1:8790", "GET", "/v1/summary"}, {"127.0.0.1:8790", "GET", "/v1/export.csv"},
		{"127.0.0.1:8790", "GET", "/healthz"}, {"127.0.0.1:8790", "GET", "/v1/no-such-endpoint"},
		{"127.0.0.1:8790", "POST", "/v1/summary"}, {"127.0.0.1:8790", "GET", "/"},
		{"127.0.0.1:8790", "POST", "/v1/ingest"}, {"127.0.0.1:8790", "POST", "/v1/enroll"},
		{"evil.example", "GET", "/"}, {"evil.example", "GET", "/assets/index-abc123.js"},
	} {
		res := send(h, tc.host, tc.method, tc.path)
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "same-origin",
		} {
			if got := res.Header.Get(k); got != want {
				t.Errorf("%s %s%s (%d): %s = %q, want %q", tc.method, tc.host, tc.path, res.StatusCode, k, got, want)
			}
		}
		if !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s %s%s: no CSP", tc.method, tc.host, tc.path)
		}
		// The page and its bundles set their own caching; nothing else may be stored.
		static := res.StatusCode == http.StatusOK && (tc.path == "/" || strings.HasPrefix(tc.path, "/assets/"))
		if !static && res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s%s (%d): Cache-Control = %q, want no-store",
				tc.method, tc.host, tc.path, res.StatusCode, res.Header.Get("Cache-Control"))
		}
	}
}

// The dashboard has no sign-in, so a server started on an address another
// machine can reach would serve it to anyone.
func TestOnlyALoopbackServerStarts(t *testing.T) {
	for _, tc := range []struct {
		addr   string
		starts bool
	}{
		{"127.0.0.1:8790", true},
		{"localhost:8790", true},
		{"[::1]:8790", true},
		{"0.0.0.0:8790", false},
		{":8790", false},
		{"192.168.1.20:8790", false},
	} {
		if err := loopbackOnly(tc.addr); (err == nil) != tc.starts {
			t.Errorf("%s: err %v, want starts = %v", tc.addr, err, tc.starts)
		}
	}
}

// The dashboard needs no sign-in, but only for a request addressed to this
// machine by name: a page elsewhere that rebinds its hostname to 127.0.0.1
// gets nothing. Uploads still need an enrolled token.
func TestTheServerAnswersOnlyRequestsAddressedToThisMachine(t *testing.T) {
	h := handler(t)
	for _, host := range []string{"127.0.0.1:8790", "localhost:5178", "[::1]:8790"} {
		for _, target := range []string{"/", "/v1/summary"} {
			if res := send(h, host, http.MethodGet, target); res.StatusCode != http.StatusOK {
				t.Errorf("%s%s: %d, want 200 with no sign-in", host, target, res.StatusCode)
			}
		}
	}
	for _, host := range []string{"evil.example", "evil.example:8790", "127.0.0.1.evil.example"} {
		if res := send(h, host, http.MethodGet, "/v1/summary"); res.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("Host %s: %d, want 421", host, res.StatusCode)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(`{"v":1,"machine_id":"m"}`))
	req.Host = "127.0.0.1:8790"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an upload with no token: %d, want 401", rec.Code)
	}
}

// -retain N rolls up only what is older than N UTC days. A cutoff on the
// wrong side of today rolls up the live window, and ingest then refuses every
// agent's current events as below the floor.
func TestPruneCutoffLeavesTheRetentionWindowRaw(t *testing.T) {
	for _, n := range []int{1, 30, 90} {
		before := time.Now().UTC()
		got := pruneCutoff(n)
		after := time.Now().UTC()
		// Either side of a midnight the call may straddle.
		if got != before.AddDate(0, 0, -n).Format(time.DateOnly) &&
			got != after.AddDate(0, 0, -n).Format(time.DateOnly) {
			t.Errorf("pruneCutoff(%d) = %s on %s, want the day %d days before", n, got,
				after.Format(time.DateOnly), n)
		}
	}
}

// A rollup freezes the costs it sums, so the startup prune must see this
// build's prices: started beside the reprice, it rolled up the last build's.
func TestTheStartupPruneRollsUpThisBuildsPrices(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	events := make([]schema.Event, 3_000)
	for i := range events {
		events[i] = schema.Event{V: schema.Version, ID: fmt.Sprint("e", i), Source: schema.SourceClaudeCode,
			TS: time.Now().AddDate(0, 0, -200), Model: "claude-opus-5", CostBasis: schema.CostBilled,
			Usage: schema.Usage{InputTokens: 1_000_000}}
	}
	if _, err := d.Ingest(ctx, "tester", &schema.Batch{V: schema.Version, MachineID: "m", Events: events}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	sum := func(table string) (usd float64) {
		t.Helper()
		if err := raw.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM ` + table).Scan(&usd); err != nil {
			t.Fatal(err)
		}
		return usd
	}
	want := sum("event")
	// As a build with other prices left them.
	if _, err := raw.Exec(`UPDATE event SET cost_usd = cost_usd * 2`); err != nil {
		t.Fatal(err)
	}

	bg, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { maintain(bg, d, 100, slog.New(slog.DiscardHandler)); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); sum("daily_rollup") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no prune ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	if got := sum("daily_rollup"); got != want {
		t.Fatalf("rolled up at $%.2f, want this build's $%.2f", got, want)
	}
}
