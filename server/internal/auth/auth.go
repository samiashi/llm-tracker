// Package auth gates the dashboard behind GitHub org membership. The org
// already lists who is on the team, so there is no second list to maintain
// and nobody to deprovision twice. Local is the gate for local development.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "llm_tracker_session"
	stateCookie   = "llm_tracker_state"
	sessionTTL    = 12 * time.Hour
)

// Config describes the OAuth app and the organisation that gates access.
// Every field is required.
type Config struct {
	ClientID     string
	ClientSecret string
	Org          string
	// BaseURL is this server's externally reachable address, used to build the
	// callback. GitHub rejects a callback that does not match the app's
	// registered URL, so this has to be the real one rather than the listen
	// address. An https:// one marks the session cookie Secure.
	BaseURL string
	// SessionKey signs session cookies. It is fixed rather than generated so
	// a restart does not sign everyone out.
	SessionKey []byte
}

type Authenticator struct {
	cfg    Config
	secure bool
	client *http.Client
	Log    *slog.Logger

	// signIns counts callbacks, each of which spends the client secret and
	// a code the caller chose on GitHub.
	signIns Limiter
}

// minSessionKeyLen is the shortest key accepted. Anything weaker is
// brute-forceable offline from a single captured cookie, after which every
// future session can be forged.
const minSessionKeyLen = 32

// New builds an authenticator, refusing an incomplete configuration or a
// weak session key rather than running with either.
func New(cfg Config) (*Authenticator, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Org == "" || cfg.BaseURL == "" {
		return nil, errors.New("GitHub auth needs an OAuth app's client ID and secret, the org, and the base URL")
	}
	if len(cfg.SessionKey) < minSessionKeyLen {
		return nil, fmt.Errorf("the session key must be at least %d bytes, got %d",
			minSessionKeyLen, len(cfg.SessionKey))
	}
	return &Authenticator{
		cfg:    cfg,
		secure: strings.HasPrefix(cfg.BaseURL, "https://"),
		client: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// User is who the session belongs to.
type User struct {
	Login string `json:"login"`
}

// exempt is every route reachable without a session: the OAuth flow, the
// health check, and the two routes an agent calls, which carry a bearer token
// because an agent has no browser to log in with -- an ingest token, or for
// enrolment the GitHub token that proves who is asking.
//
// Exact routes, keyed as the mux matches them -- method and escaped path --
// so that no other method or spelling of these paths, which the mux sends to
// the page instead, passes too.
var exempt = map[string]bool{
	"GET /auth/login":    true,
	"GET /auth/callback": true,
	"GET /auth/logout":   true,
	"GET /healthz":       true,
	"POST /v1/ingest":    true,
	"POST /v1/enroll":    true,
}

// Middleware requires a valid session for everything but the exempt routes.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path := r.Method, r.URL.EscapedPath()
		if method == http.MethodHead {
			method = http.MethodGet // the mux serves HEAD with a GET route
		}
		if exempt[method+" "+path] {
			next.ServeHTTP(w, r)
			return
		}

		if _, ok := a.session(r); ok {
			next.ServeHTTP(w, r)
			return
		}

		// An API caller gets a status it can act on; a browser gets sent to
		// the login it was going to need anyway.
		if strings.HasPrefix(path, "/v1/") {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	})
}

// Routes registers the OAuth endpoints.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/logout", a.handleLogout)
}

func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	// State ties the callback to this browser, so a third party cannot feed us
	// a code they obtained elsewhere.
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	state := base64.RawURLEncoding.EncodeToString(raw)
	next := r.URL.Query().Get("next")

	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state + "|" + next, Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: 600,
	})

	q := url.Values{
		"client_id":    {a.cfg.ClientID},
		"redirect_uri": {a.callbackURL()},
		// read:org is needed to check membership of a private organisation.
		"scope": {"read:org"},
		"state": {state},
	}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	// Both halves must be non-empty before they are compared:
	// ConstantTimeCompare returns 1 for two empty slices, and any sibling host
	// under the same registrable domain can plant an empty state cookie, then
	// have the victim's browser complete a login with the attacker's code.
	want, next, ok := strings.Cut(c.Value, "|")
	got := r.URL.Query().Get("state")
	if !ok || want == "" || got == "" ||
		subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	// The state proves nothing about the caller, who can set both halves.
	if !a.signIns.Allow(r) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many sign-ins at once; try again in a minute", http.StatusTooManyRequests)
		return
	}

	token, err := a.exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		a.fail(w, "oauth exchange failed", err, http.StatusBadGateway)
		return
	}
	user, err := fetchUser(r.Context(), a.client, token)
	if err != nil {
		a.fail(w, "cannot read GitHub user", err, http.StatusBadGateway)
		return
	}
	member, err := a.isOrgMember(r.Context(), token, user.Login)
	if err != nil {
		a.fail(w, "cannot check org membership", err, http.StatusBadGateway)
		return
	}
	if !member {
		http.Error(w, fmt.Sprintf("%s is not a member of %s", user.Login, a.cfg.Org),
			http.StatusForbidden)
		return
	}

	a.setSession(w, user)
	//nolint:gosec // G710: safeNext rejects anything but a local path.
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

// safeNext restricts a post-login redirect to a path on this host.
//
// A leading "/" is not enough: Go emits "//evil.com" as a scheme-relative
// Location that browsers resolve off-site, and "/\evil.com" is the same trick
// via a backslash several browsers normalise to "//". The OAuth round-trip is
// silent for anyone signed in to GitHub, so a trusted login URL would land
// them on a phishing page.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return "/"
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

// fail reports a generic message to the browser and keeps the upstream detail
// in the log, so GitHub's error text is not reflected back to a visitor.
func (a *Authenticator) fail(w http.ResponseWriter, msg string, err error, code int) {
	if a.Log != nil {
		a.Log.Error("auth", "msg", msg, "err", err)
	}
	http.Error(w, msg, code)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// callbackURL is where GitHub sends the browser back to. The token exchange
// must name the one the login did, or GitHub refuses it.
func (a *Authenticator) callbackURL() string {
	return strings.TrimRight(a.cfg.BaseURL, "/") + "/auth/callback"
}

func (a *Authenticator) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {a.callbackURL()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("no token returned: %s", body.Error)
	}
	return body.AccessToken, nil
}

// Member reports who a GitHub token belongs to and whether they are in the
// org, with an empty login when GitHub rejects the token itself. It is how an
// agent enrols: the token the GitHub CLI already holds on a colleague's
// machine proves who they are, so nobody has to hand them a secret. The token
// is used for these two reads and is neither kept nor logged.
func (a *Authenticator) Member(ctx context.Context, token string) (login string, member bool, err error) {
	u, err := fetchUser(ctx, a.client, token)
	if errors.Is(err, errRejected) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	member, err = a.isOrgMember(ctx, token, u.Login)
	return u.Login, member, err
}

// errRejected is GitHub refusing the token outright.
var errRejected = errors.New("github rejected the token")

// fetchUser names the GitHub user a token belongs to.
func fetchUser(ctx context.Context, client *http.Client, token string) (User, error) {
	var u User
	err := getJSON(ctx, client, token, "https://api.github.com/user", &u)
	if err == nil && u.Login == "" {
		err = errors.New("github returned a user with no login")
	}
	return u, err
}

// isOrgMember checks membership directly rather than listing the caller's
// organisations, so a user in many orgs costs one request either way and the
// answer cannot be truncated by pagination.
func (a *Authenticator) isOrgMember(ctx context.Context, token, login string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/orgs/%s/members/%s",
			url.PathEscape(a.cfg.Org), url.PathEscape(login)), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("github returned %d", resp.StatusCode)
	}
}

func getJSON(ctx context.Context, client *http.Client, token, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errRejected
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// setSession issues an HMAC-signed cookie. The payload is not secret -- it is
// a login name and an expiry -- but it must not be forgeable.
func (a *Authenticator) setSession(w http.ResponseWriter, u User) {
	exp := time.Now().Add(sessionTTL)
	body, err := json.Marshal(sessionPayload{Login: u.Login, Expires: exp.Unix()})
	if err != nil {
		return
	}
	value := base64.RawURLEncoding.EncodeToString(body) + "." + a.sign(string(body))

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
		Expires: exp,
	})
}

func (a *Authenticator) session(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return User{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return User{}, false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(a.sign(string(raw)))) != 1 {
		return User{}, false
	}
	var p sessionPayload
	if json.Unmarshal(raw, &p) != nil {
		return User{}, false
	}
	if p.Login == "" || time.Now().Unix() > p.Expires {
		return User{}, false
	}
	return User{Login: p.Login}, true
}

// sessionPayload is the signed cookie body.
type sessionPayload struct {
	Login   string `json:"l"`
	Expires int64  `json:"e"`
}

func (a *Authenticator) sign(payload string) string {
	m := hmac.New(sha256.New, a.cfg.SessionKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
