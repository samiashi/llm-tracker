package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

const githubToken = "gho_colleague"

func TestEnrollReturnsTheTokenTheServerIssued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in schema.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer "+githubToken ||
			r.Header.Get("Content-Type") != "application/json" || in.Hostname != "laptop" {
			t.Errorf("request: %s %s, %+v", r.URL.Path, r.Header, in)
		}
		_ = json.NewEncoder(w).Encode(schema.EnrollResponse{
			Token: schema.EnrolledTokenPrefix + "issued", Login: "alice",
		})
	}))
	defer srv.Close()

	got, err := Enroll(context.Background(), srv.URL, githubToken, schema.EnrollRequest{Hostname: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != schema.EnrolledTokenPrefix+"issued" || got.Login != "alice" {
		t.Fatalf("Enroll = %+v", got)
	}
}

// The GitHub token opens everything its owner can reach: it may cross a
// network only under TLS.
func TestEnrollSendsTheGitHubTokenOnlyOverTLSOrToLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"https://tracker.example.com": true,
		"http://127.0.0.1:8790":       true,
		"http://[::1]:8790":           true,
		"http://localhost:8790":       true,
		"http://tracker.example.com":  false,
		"http://10.0.0.5:8790":        false,
		"ftp://tracker.example.com":   false,
	} {
		if err := RequireTLS(addr); (err == nil) != ok {
			t.Errorf("RequireTLS(%s) = %v, want allowed = %v", addr, err, ok)
		}
	}
	if _, err := Enroll(context.Background(), "http://tracker.example.com", githubToken,
		schema.EnrollRequest{}); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("Enroll over plain http: %v", err)
	}
}

// A redirect would hand the token to whatever it names.
func TestEnrollFollowsNoRedirect(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed, with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/enroll", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	_, err := Enroll(context.Background(), srv.URL, githubToken, schema.EnrollRequest{})
	if err == nil || !strings.Contains(err.Error(), "redirecting") {
		t.Fatalf("Enroll through a redirect: %v", err)
	}
}

func TestEnrollSaysWhyTheServerRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		says   string
	}{
		{"GitHub rejected the token", 401, `{"error":"GitHub rejected the token"}`, "server returned 401: GitHub rejected the token"},
		{"no enrolment", 404, `{"error":"this server does not enrol agents"}`, "does not enrol"},
		// A dashboard page, or anything else answering 200, is not a token.
		{"not the tracker", 200, "<!doctype html>", "did not answer with an enrolment"},
		{"a token of another kind", 200, `{"token":"shared","login":"alice"}`, "did not answer with an enrolment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := Enroll(context.Background(), srv.URL, githubToken, schema.EnrollRequest{})
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("error %v, want it to say %q", err, tc.says)
			}
			if strings.Contains(err.Error(), githubToken) {
				t.Fatalf("the error repeats the GitHub token: %v", err)
			}
		})
	}
}
