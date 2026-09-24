// Package auth decides who may use the server. It listens on loopback and
// serves only requests addressed to this machine, so the dashboard needs no
// sign-in. Enrolment asks GitHub who a token belongs to, so every machine
// belongs to a real login.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Local is the server's gate: only this machine can connect to a server
// bound to loopback, so there is no sign-in and no list of who may enter.
type Local struct {
	client *http.Client
}

// NewLocal builds the gate.
func NewLocal() *Local {
	return &Local{client: &http.Client{Timeout: 15 * time.Second}}
}

// Middleware serves a request only when it is addressed to this machine by
// name. Any page on the web can point its own hostname at 127.0.0.1, and the
// browser would then hand that page the dashboard's replies as its own.
func (*Local) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLoopback(r.Host) {
			http.Error(w, "this server answers only to localhost, 127.0.0.1 and [::1]",
				http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Login names the GitHub user a token belongs to, with an empty login when
// GitHub rejects the token. It is how an agent enrols: the token the GitHub
// CLI already holds proves who is asking, so nobody has to hand out a
// secret. The token is spent on this one read and neither kept nor logged.
func (l *Local) Login(ctx context.Context, token string) (string, error) {
	u, err := fetchUser(ctx, l.client, token)
	if errors.Is(err, errRejected) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return u.Login, nil
}

// IsLoopback reports whether a host, with or without its port, names this
// machine: localhost or a loopback address.
func IsLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// user is who a GitHub token belongs to.
type user struct {
	Login string `json:"login"`
}

// errRejected is GitHub refusing the token outright.
var errRejected = errors.New("github rejected the token")

// fetchUser names the GitHub user a token belongs to.
func fetchUser(ctx context.Context, client *http.Client, token string) (user, error) {
	var u user
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return u, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return u, errRejected
	}
	if resp.StatusCode >= 300 {
		return u, fmt.Errorf("github returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return u, err
	}
	if u.Login == "" {
		return u, errors.New("github returned a user with no login")
	}
	return u, nil
}
