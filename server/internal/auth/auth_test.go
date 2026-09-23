package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := New(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Org:          "your-org",
		BaseURL:      "https://usage.example.com",
		SessionKey:   []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tests never reach GitHub: every call it would make fails here, so a
	// test that gets as far as the token exchange sees a failed exchange.
	a.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("no network in tests: %s", r.URL.Host)
	})}
	return a
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// State is what stops a victim's browser completing a login with someone
// else's authorization code. The empty cases matter most: ConstantTimeCompare
// returns 1 for two empty slices.
func TestCallbackRejectsEveryStateButTheMatchingOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie string // "" means send no cookie at all
		query  string
		want   int
	}{
		{"no cookie", "", "anything", http.StatusBadRequest},
		{"mismatched", "real|/", "wrong", http.StatusBadRequest},
		{"empty cookie, empty param", "", "", http.StatusBadRequest},
		{"empty value, empty param", "|/", "", http.StatusBadRequest},
		{"empty value, some param", "|/", "abc", http.StatusBadRequest},
		{"real value, empty param", "real|/", "", http.StatusBadRequest},
		{"no separator", "real", "real", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAuth(t)
			req := httptest.NewRequest("GET", "/auth/callback?code=c&state="+tc.query, nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: stateCookie, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			a.handleCallback(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)",
					rec.Code, tc.want, rec.Body.String())
			}
			// A 502 means the state check passed and the exchange ran.
			if rec.Code == http.StatusBadGateway {
				t.Fatal("the state check was bypassed and the token exchange ran")
			}
		})
	}
}

// A matching state must still be accepted, or the test above would pass on a
// handler that rejected everything.
func TestCallbackAcceptsAMatchingState(t *testing.T) {
	a := testAuth(t)
	req := httptest.NewRequest("GET", "/auth/callback?code=c&state=match", nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: "match|/"})
	rec := httptest.NewRecorder()
	a.handleCallback(rec, req)

	// The exchange cannot reach GitHub here, so it fails: a 502 proves the
	// request got past the state check.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 -- a matching state should reach the exchange", rec.Code)
	}
}

// next comes back from the login redirect, so it is attacker-controlled.
func TestSafeNextRefusesAnythingButALocalPath(t *testing.T) {
	cases := map[string]string{
		"/":                       "/",
		"/agents?from=2026-01-01": "/agents?from=2026-01-01",
		"":                        "/",
		"//evil.example":          "/",
		`/\evil.example`:          "/",
		"https://evil.example":    "/",
		"javascript:alert(1)":     "/",
		"evil.example":            "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// Any other spelling -- a prefix, an escaped slash, another method -- reaches
// a handler that needs a session.
func TestMiddlewareExemptsExactRoutesOnly(t *testing.T) {
	a := testAuth(t)
	reached := false
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct {
		method, path string
		wantThru     bool
		wantCode     int
	}{
		{"GET", "/auth/login", true, http.StatusOK},
		{"GET", "/auth/callback", true, http.StatusOK},
		{"GET", "/auth/logout", true, http.StatusOK},
		{"GET", "/healthz", true, http.StatusOK},
		{"HEAD", "/healthz", true, http.StatusOK},
		{"POST", "/v1/ingest", true, http.StatusOK},
		{"POST", "/v1/enroll", true, http.StatusOK},

		{"GET", "/auth/", false, http.StatusFound},
		{"GET", "/auth/anything", false, http.StatusFound},
		{"GET", "/auth/../v1/agents", false, http.StatusFound},
		{"GET", "/auth/me", false, http.StatusFound},
		{"GET", "/v1%2Fingest", false, http.StatusFound},
		{"POST", "/v1%2Fingest", false, http.StatusFound},
		{"GET", "/auth%2Flogin", false, http.StatusFound},
		{"GET", "/healthz%2F", false, http.StatusFound},
		{"POST", "/auth/login", false, http.StatusFound},
		{"POST", "/healthz", false, http.StatusFound},
		{"GET", "/v1/ingest", false, http.StatusUnauthorized},
		{"GET", "/v1/enroll", false, http.StatusUnauthorized},
		{"POST", "/v1%2Fenroll", false, http.StatusFound},

		{"GET", "/v1/agents", false, http.StatusUnauthorized},
		{"GET", "/v1/summary", false, http.StatusUnauthorized},
		{"GET", "/", false, http.StatusFound},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			reached = false
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if reached != tc.wantThru {
				t.Errorf("reached handler = %v, want %v", reached, tc.wantThru)
			}
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

// There is no unauthenticated mode to fall back to, so a missing setting is
// a refusal to start.
func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	full := Config{ClientID: "id", ClientSecret: "secret", Org: "o", BaseURL: "https://x",
		SessionKey: []byte("0123456789abcdef0123456789abcdef")}
	for name, drop := range map[string]func(*Config){
		"client ID":     func(c *Config) { c.ClientID = "" },
		"client secret": func(c *Config) { c.ClientSecret = "" },
		"org":           func(c *Config) { c.Org = "" },
		"base URL":      func(c *Config) { c.BaseURL = "" },
		"session key":   func(c *Config) { c.SessionKey = nil },
	} {
		c := full
		drop(&c)
		if _, err := New(c); err == nil {
			t.Errorf("New accepted a config without a %s", name)
		}
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New refused a complete config: %v", err)
	}
}

// A session cookie is a bearer credential. It must survive a round trip, and
// nothing else may be accepted as one.
func TestSessionRoundTripsAndRejectsTampering(t *testing.T) {
	a := testAuth(t)
	rec := httptest.NewRecorder()
	a.setSession(rec, User{Login: "sam"})

	set := rec.Result().Cookies()
	if len(set) == 0 {
		t.Fatal("no cookie was set")
	}
	c := set[0]
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure under an https base URL")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	u, ok := a.session(req)
	if !ok || u.Login != "sam" {
		t.Fatalf("session did not round trip: %+v ok=%v", u, ok)
	}

	for _, bad := range []string{
		c.Value + "x",    // corrupted signature
		"forged.payload", // not ours
		"",               // empty
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: bad})
		if _, ok := a.session(req); ok {
			t.Errorf("accepted a tampered session cookie: %q", bad)
		}
	}
}

// Signed with the real key, so the expiry check is what refuses it.
func TestExpiredSessionIsRefusedDespiteAValidSignature(t *testing.T) {
	a := testAuth(t)

	body, err := json.Marshal(sessionPayload{
		Login: "sam", Expires: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	value := base64.RawURLEncoding.EncodeToString(body) + "." + a.sign(string(body))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	if _, ok := a.session(req); ok {
		t.Fatal("an expired session was accepted")
	}

	// The same payload one minute the other side of now must be accepted, or
	// this test would pass on a handler that rejected everything.
	body, _ = json.Marshal(sessionPayload{
		Login: "sam", Expires: time.Now().Add(time.Minute).Unix(),
	})
	value = base64.RawURLEncoding.EncodeToString(body) + "." + a.sign(string(body))
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	if _, ok := a.session(req); !ok {
		t.Fatal("an unexpired, correctly signed session was refused")
	}
}

// Sessions issued while the payload carried the display name ("n") are still
// signed by the same key, and must keep working until they expire.
func TestASessionIssuedWithADisplayNameStillValidates(t *testing.T) {
	a := testAuth(t)
	body := fmt.Sprintf(`{"l":"sam","n":"Sam | Example","e":%d}`, time.Now().Add(time.Hour).Unix())
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie,
		Value: base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + a.sign(body)})
	if u, ok := a.session(req); !ok || u.Login != "sam" {
		t.Fatalf("session = %+v, %v; want sam's", u, ok)
	}
}

// The dashboard's own gate: GitHub vouches for the login, the org check
// admits only members, and the way back after it lands on this host only.
func TestCallbackSignsInOnlyMembersAndOnlyToALocalPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		membership  int
		next        string
		wantCode    int
		wantTo      string
		wantSession bool
	}{
		{"a member", http.StatusNoContent, "/agents", http.StatusFound, "/agents", true},
		{"a member sent off-site", http.StatusNoContent, "//evil.example", http.StatusFound, "/", true},
		{"not a member", http.StatusNotFound, "/", http.StatusForbidden, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAuth(t)
			a.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				code, body := http.StatusInternalServerError, ""
				switch r.URL.Host + r.URL.Path {
				case "github.com/login/oauth/access_token":
					code, body = http.StatusOK, `{"access_token":"gho_alice"}`
				case "api.github.com/user":
					code, body = http.StatusOK, `{"login":"alice"}`
				case "api.github.com/orgs/your-org/members/alice":
					code = tc.membership
				default:
					t.Errorf("unexpected request to %s", r.URL)
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)),
					Header: http.Header{}, Request: r}, nil
			})}
			req := httptest.NewRequest("GET", "/auth/callback?code=c&state=s", nil)
			req.AddCookie(&http.Cookie{Name: stateCookie, Value: "s|" + tc.next})
			rec := httptest.NewRecorder()
			a.handleCallback(rec, req)

			var session *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					session = c
				}
			}
			if rec.Code != tc.wantCode || (session != nil) != tc.wantSession ||
				(tc.wantTo != "" && rec.Header().Get("Location") != tc.wantTo) {
				t.Fatalf("status %d, session %v, Location %q; want %d, %v, %q",
					rec.Code, session != nil, rec.Header().Get("Location"),
					tc.wantCode, tc.wantSession, tc.wantTo)
			}
			if session != nil {
				req := httptest.NewRequest("GET", "/", nil)
				req.AddCookie(session)
				if u, ok := a.session(req); !ok || u.Login != "alice" {
					t.Fatalf("the session set is %+v, %v; want alice's", u, ok)
				}
			}
		})
	}
}

// A short key is a silent downgrade of every session signature, so New must
// refuse it rather than pad it.
func TestNewRefusesAShortSessionKey(t *testing.T) {
	_, err := New(Config{
		ClientID: "id", ClientSecret: "secret", Org: "o",
		BaseURL: "https://x", SessionKey: []byte("tooshort"),
	})
	if err == nil {
		t.Fatal("New accepted a session key shorter than minSessionKeyLen")
	}
}

// github answers Member's two requests with the given statuses, as alice, and
// fails the test if the token is sent anywhere but the GitHub API.
func github(t *testing.T, a *Authenticator, user, membership int) {
	t.Helper()
	a.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer gho_colleague" {
			t.Errorf("request to %s with Authorization %q", r.URL, r.Header.Get("Authorization"))
		}
		code, body := membership, ""
		switch r.URL.Path {
		case "/user":
			code, body = user, `{"login":"alice"}`
		case "/orgs/your-org/members/alice":
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{}, Request: r}, nil
	})}
}

// Enrolment admits exactly who the dashboard does: a member of the org.
func TestMemberAdmitsOnlyOrgMembers(t *testing.T) {
	for _, tc := range []struct {
		name             string
		user, membership int
		login            string
		member, err      bool
	}{
		{"member", 200, 204, "alice", true, false},
		{"not a member", 200, 404, "alice", false, false},
		// No login at all: the caller answers 401, not 403.
		{"token rejected", 401, 0, "", false, false},
		{"user lookup fails", 500, 0, "", false, true},
		// Neither yes nor no, so no token is issued on it.
		{"membership hidden", 200, 403, "alice", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAuth(t)
			github(t, a, tc.user, tc.membership)
			login, member, err := a.Member(context.Background(), "gho_colleague")
			if login != tc.login || member != tc.member || (err != nil) != tc.err {
				t.Fatalf("Member = %q, %v, %v; want %q, %v, error %v",
					login, member, err, tc.login, tc.member, tc.err)
			}
		})
	}
}
