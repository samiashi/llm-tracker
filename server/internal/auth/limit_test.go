package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// One address runs out of attempts on its own; the rest share what is left
// under the cap on all of them; the next minute starts clean.
func TestAttemptsAreLimitedPerAddressAndInAll(t *testing.T) {
	var l Limiter
	t0 := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for i := range perAddress {
		if !l.allow("a", t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("attempt %d from one address refused inside its limit", i)
		}
	}
	if l.allow("a", t0.Add(59*time.Second)) {
		t.Fatal("an address went past its limit")
	}
	admitted := perAddress
	for i := 0; admitted < perMinute; i++ {
		if !l.allow(fmt.Sprint("other-", i), t0) {
			t.Fatalf("a fresh address was refused with %d of %d attempts used", admitted, perMinute)
		}
		admitted++
	}
	if l.allow("yet another", t0) {
		t.Fatal("an attempt went past the cap on all addresses")
	}
	if !l.allow("a", t0.Add(time.Minute)) {
		t.Fatal("the next minute did not start clean")
	}
}

// Behind Caddy every connection comes from Caddy, and the client is the entry
// Caddy wrote last; a client's own entries sit to its left.
func TestAnAttemptCountsAgainstTheAddressCaddySaw(t *testing.T) {
	for _, tc := range []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct", "203.0.113.7:51000", nil, "203.0.113.7"},
		{"through caddy", "172.18.0.2:41234", []string{"203.0.113.7"}, "203.0.113.7"},
		{"a client's own entry", "172.18.0.2:41234", []string{"1.2.3.4, 203.0.113.7"}, "203.0.113.7"},
		{"two header lines", "172.18.0.2:41234", []string{"1.2.3.4", "203.0.113.7"}, "203.0.113.7"},
		{"an entry with a port", "172.18.0.2:41234", []string{"203.0.113.7:4711"}, "203.0.113.7"},
		{"IPv6 by its /64", "[2001:db8:1:2:aaaa::1]:51000", nil, "2001:db8:1:2::/64"},
		{"another host in that /64", "172.18.0.2:41234", []string{"2001:db8:1:2:bbbb::9"}, "2001:db8:1:2::/64"},
		{"IPv4 mapped into IPv6", "[::ffff:203.0.113.7]:51000", nil, "203.0.113.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientAddr(r); got != tc.want {
				t.Fatalf("clientAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// Each callback spends the client secret on GitHub with a code the caller
// chose, and the state check stops no one who sets both halves themselves.
func TestSignInIsRateLimitedBeforeGitHubIsAsked(t *testing.T) {
	a := testAuth(t)
	asked := 0
	a.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		asked++
		return nil, fmt.Errorf("no network in tests: %s", r.URL.Host)
	})}
	callback := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/auth/callback?code=c&state=s", nil)
		req.AddCookie(&http.Cookie{Name: stateCookie, Value: "s|/"})
		rec := httptest.NewRecorder()
		a.handleCallback(rec, req)
		return rec
	}
	for i := range perAddress {
		if rec := callback(); rec.Code != http.StatusBadGateway {
			t.Fatalf("callback %d: %d, want it to reach the exchange", i, rec.Code)
		}
	}
	rec := callback()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("past the limit: %d, Retry-After %q; want 429 with one", rec.Code, rec.Header().Get("Retry-After"))
	}
	if asked != perAddress {
		t.Fatalf("GitHub asked %d times for %d admitted callbacks", asked, perAddress)
	}
}
