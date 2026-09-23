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
	"slices"
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

	authn, err := auth.New(auth.Config{
		ClientID: "id", ClientSecret: "secret", Org: "org", BaseURL: "http://127.0.0.1:8790",
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{DB: d, Log: slog.New(slog.DiscardHandler), Version: "v1.0.0", Enroll: authn}
	return newHandler(srv, authn, web.NewHandler(fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>dashboard</title>")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}))
}

// reply is what a client received: the status and the header as sent.
type reply struct {
	StatusCode int
	Header     http.Header
}

// send serves one request carrying what an agent's upload carries.
func send(h http.Handler, method, target string) reply {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"v":1,"machine_id":"m","events":[]}`)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	return reply{res.StatusCode, res.Header}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := handler(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/summary"}, {"GET", "/v1/export.csv"}, {"GET", "/healthz"},
		{"GET", "/auth/login"}, {"GET", "/auth/logout"}, {"GET", "/auth/callback"},
		{"GET", "/v1/no-such-endpoint"}, {"POST", "/v1/summary"}, {"GET", "/"},
		{"POST", "/v1/ingest"}, {"POST", "/v1/enroll"},
	} {
		res := send(h, tc.method, tc.path)
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "same-origin",

			"Strict-Transport-Security": "max-age=31536000",
		} {
			if got := res.Header.Get(k); got != want {
				t.Errorf("%s %s (%d): %s = %q, want %q", tc.method, tc.path, res.StatusCode, k, got, want)
			}
		}
		if !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s %s: no CSP", tc.method, tc.path)
		}
		if res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s (%d): Cache-Control = %q, want no-store",
				tc.method, tc.path, res.StatusCode, res.Header.Get("Cache-Control"))
		}
		if !slices.Contains(res.Header.Values("Vary"), "Cookie") {
			t.Errorf("%s %s: Vary = %v, want Cookie", tc.method, tc.path, res.Header.Values("Vary"))
		}
	}
}

// Without a session the bundle's immutable caching must not reach the login
// redirect that stands in for it.
func TestStaticCachingNeverReachesTheLoginRedirect(t *testing.T) {
	h := handler(t)
	for _, path := range []string{"/assets/index-abc123.js", "/", "/v1/summary"} {
		res := send(h, "GET", path)
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s (%d): Cache-Control = %q, want no-store", path, res.StatusCode, got)
		}
	}
}

func TestNoOtherSpellingOfAnExemptRouteServesThePage(t *testing.T) {
	h := handler(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1%2Fingest"}, {"GET", "/auth%2Flogin"}, {"POST", "/auth/login"}, {"POST", "/healthz"},
	} {
		res := send(h, tc.method, tc.path)
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s %s was served (%s) without a session", tc.method, tc.path,
				res.Header.Get("Content-Type"))
		}
	}
}

// clearAuth unsets every GitHub auth setting for the test.
func clearAuth(t *testing.T) {
	t.Helper()
	for _, k := range authSettings {
		t.Setenv(k, "")
	}
}

// Some settings but not all is a deployment short of one, never local
// development: the server names every one that is missing instead of starting.
func TestAHalfConfiguredServerNamesWhatIsMissing(t *testing.T) {
	clearAuth(t)
	t.Setenv("LLM_TRACKER_GITHUB_ORG", "your-org")
	_, err := dashboardGate("127.0.0.1:8790", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a server with only the org set started, on loopback")
	}
	for _, k := range []string{"CLIENT_ID", "CLIENT_SECRET", "BASE_URL", "SESSION_KEY"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("%v does not name %s", err, k)
		}
	}
	if strings.Contains(err.Error(), "GITHUB_ORG") {
		t.Errorf("%v names the org, which is set", err)
	}
}

// With no GitHub app, a server serves only this machine: it starts on a
// loopback address, and refuses any address another machine could reach.
func TestWithNoGitHubAppOnlyALoopbackServerStarts(t *testing.T) {
	clearAuth(t)
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
		g, err := dashboardGate(tc.addr, slog.New(slog.DiscardHandler))
		if started := err == nil; started != tc.starts {
			t.Errorf("%s: started %v (err %v), want %v", tc.addr, started, err, tc.starts)
		}
		if err == nil {
			if _, ok := g.(*auth.Local); !ok {
				t.Errorf("%s: gate %T, want the local one", tc.addr, g)
			}
		}
	}
}

// Locally the dashboard needs no sign-in, but only for a request addressed to
// this machine by name: a page elsewhere that rebinds its hostname to
// 127.0.0.1 gets nothing. Uploads still need an enrolled token.
func TestALocalServerServesOnlyRequestsAddressedToThisMachine(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	g := auth.NewLocal()
	srv := &api.Server{DB: d, Log: slog.New(slog.DiscardHandler), Version: "v1.0.0", Enroll: g}
	h := newHandler(srv, g, web.NewHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>dashboard</title>")},
	}))

	get := func(host, target string) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, host := range []string{"127.0.0.1:8790", "localhost:5178", "[::1]:8790"} {
		for _, target := range []string{"/", "/v1/summary"} {
			if code := get(host, target); code != http.StatusOK {
				t.Errorf("%s%s: %d, want 200 with no sign-in", host, target, code)
			}
		}
	}
	for _, host := range []string{"evil.example", "evil.example:8790", "127.0.0.1.evil.example"} {
		if code := get(host, "/v1/summary"); code != http.StatusMisdirectedRequest {
			t.Errorf("Host %s: %d, want 421", host, code)
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
