package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A loopback server serves no org, so enrolment admits whoever GitHub names,
// and still refuses a token GitHub itself rejects. Only /user is asked.
func TestLocalEnrolmentAdmitsWhoeverGitHubNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantLogin string
		wantErr   bool
	}{
		{"a token GitHub names", http.StatusOK, "alice", false},
		{"a token GitHub rejects", http.StatusUnauthorized, "", false},
		{"GitHub failing", http.StatusBadGateway, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLocal()
			l.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" || r.URL.Path != "/user" ||
					r.Header.Get("Authorization") != "Bearer gho_colleague" {
					t.Errorf("request to %s with Authorization %q", r.URL, r.Header.Get("Authorization"))
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Request: r,
					Body: io.NopCloser(strings.NewReader(`{"login":"alice"}`))}, nil
			})}
			login, member, err := l.Member(context.Background(), "gho_colleague")
			if (err != nil) != tc.wantErr || login != tc.wantLogin || member != (tc.wantLogin != "") {
				t.Fatalf("Member = (%q, %v, %v), want login %q", login, member, err, tc.wantLogin)
			}
		})
	}
}

// IsLoopback decides both where a local server may listen and which requests
// it serves, so a name that merely starts like a loopback one must fail.
func TestIsLoopbackNamesOnlyThisMachine(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8790":         true,
		"127.0.0.1":              true,
		"127.1.2.3:80":           true,
		"localhost:5178":         true,
		"LOCALHOST":              true,
		"[::1]:8790":             true,
		"[::1]":                  true,
		"":                       false,
		":8790":                  false,
		"0.0.0.0:8790":           false,
		"192.168.1.20:8790":      false,
		"evil.example":           false,
		"127.0.0.1.evil.example": false,
		"localhost.evil.example": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}
