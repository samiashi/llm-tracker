package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// Local is the gate for local development: a server bound to loopback with no
// GitHub app configured. Only this machine can connect to it, so there is no
// sign-in and no org. Enrolment still names the GitHub user a token belongs
// to, so every machine belongs to a real login, as it does in production.
type Local struct {
	client *http.Client
}

// NewLocal builds the gate for a loopback server with no GitHub app.
func NewLocal() *Local {
	return &Local{client: &http.Client{Timeout: 15 * time.Second}}
}

// Routes registers nothing: there is no sign-in to route.
func (*Local) Routes(*http.ServeMux) {}

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

// Member names the GitHub user a token belongs to and admits them, with an
// empty login when GitHub rejects the token: a loopback server serves no org,
// and only this machine can ask.
func (l *Local) Member(ctx context.Context, token string) (login string, member bool, err error) {
	u, err := fetchUser(ctx, l.client, token)
	if errors.Is(err, errRejected) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return u.Login, true, nil
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
