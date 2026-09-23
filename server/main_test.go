package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

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

// send serves one request addressed to host, carrying what an agent's
// upload carries.
func send(h http.Handler, method, target, host string) reply {
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

const local = "127.0.0.1:8790"

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := handler(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/summary"}, {"GET", "/v1/export.csv"}, {"GET", "/healthz"},
		{"GET", "/auth/login"}, {"GET", "/auth/logout"}, {"GET", "/auth/callback"},
		{"GET", "/v1/no-such-endpoint"}, {"POST", "/v1/summary"}, {"GET", "/"},
		{"POST", "/v1/ingest"}, {"POST", "/v1/enroll"},
	} {
		res := send(h, tc.method, tc.path, local)
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "same-origin",
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
		res := send(h, "GET", path, local)
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
		res := send(h, tc.method, tc.path, local)
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s %s was served (%s) without a session", tc.method, tc.path,
				res.Header.Get("Content-Type"))
		}
	}
}

// There is no mode without GitHub auth, so the server names every setting it
// is missing instead of starting.
func TestTheServerNamesEveryMissingAuthSetting(t *testing.T) {
	for _, k := range []string{"LLM_TRACKER_GITHUB_CLIENT_ID", "LLM_TRACKER_GITHUB_CLIENT_SECRET",
		"LLM_TRACKER_GITHUB_ORG", "LLM_TRACKER_BASE_URL", "LLM_TRACKER_SESSION_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("LLM_TRACKER_GITHUB_ORG", "your-org")
	_, err := authConfig()
	if err == nil {
		t.Fatal("authConfig accepted an environment with no GitHub auth")
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
