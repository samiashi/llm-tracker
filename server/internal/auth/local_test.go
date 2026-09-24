package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Enrolment admits whoever GitHub names, and refuses a token GitHub itself
// rejects. Only /user is asked.
func TestEnrolmentAdmitsWhoeverGitHubNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantLogin string
		wantErr   bool
	}{
		{"a token GitHub names", http.StatusOK, `{"login":"alice"}`, "alice", false},
		{"a token GitHub rejects", http.StatusUnauthorized, `{"login":"alice"}`, "", false},
		{"GitHub failing", http.StatusBadGateway, `{"login":"alice"}`, "", true},
		{"a user with no login", http.StatusOK, `{}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLocal()
			l.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" || r.URL.Path != "/user" ||
					r.Header.Get("Authorization") != "Bearer gho_colleague" {
					t.Errorf("request to %s with Authorization %q", r.URL, r.Header.Get("Authorization"))
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Request: r,
					Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			login, err := l.Login(context.Background(), "gho_colleague")
			if (err != nil) != tc.wantErr || login != tc.wantLogin {
				t.Fatalf("Login = (%q, %v), want login %q", login, err, tc.wantLogin)
			}
		})
	}
}

// IsLoopback decides both where the server may listen and which requests it
// serves, so a name that merely starts like a loopback one must fail.
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
