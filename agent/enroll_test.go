package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/schema"
)

const colleagueGitHubToken = "gho_colleague"

// fakeGH puts a gh on PATH that holds colleagueGitHubToken, and fails unless
// it is asked for exactly that token, with the host pinned.
func fakeGH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$*" = "auth token --hostname github.com" ] && [ "$GH_HOST" = "github.com" ] ||
  { echo "unexpected: gh $* (GH_HOST=$GH_HOST)" >&2; exit 2; }
echo ` + colleagueGitHubToken + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// tracker enrols anyone presenting colleagueGitHubToken, issuing issued, and
// accepts uploads from the tokens in live.
type tracker struct {
	*httptest.Server
	issued    string
	live      map[string]bool
	enrolled  atomic.Int32
	sawGitHub atomic.Bool // the GitHub token reached anything but /v1/enroll
}

func newTracker(t *testing.T, issued string, live ...string) *tracker {
	tr := &tracker{issued: issued, live: map[string]bool{issued: true}}
	for _, tok := range live {
		tr.live[tok] = true
	}
	tr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/v1/enroll":
			if bearer != colleagueGitHubToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tr.enrolled.Add(1)
			_ = json.NewEncoder(w).Encode(schema.EnrollResponse{
				Token: tr.issued, Login: "alice", ServerVersion: version,
			})
		case "/v1/ingest":
			if bearer == colleagueGitHubToken {
				tr.sawGitHub.Store(true)
			}
			if !tr.live[bearer] {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid token"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(schema.IngestAck{ServerVersion: version})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tr.Close)
	return tr
}

func TestEnrollSavesATokenOfTheMachinesOwn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGH(t)
	tr := newTracker(t, schema.EnrolledTokenPrefix+"issued")
	dir := t.TempDir()

	out, err := agentRun(t, "enroll", "-data", dir, "-server", tr.URL)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !strings.Contains(out, "enrolled as alice") {
		t.Errorf("enroll printed %q; want it to name the login", out)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != tr.URL || cfg.Token != tr.issued {
		t.Fatalf("saved %q / %q, want %q with the issued token", cfg.ServerURL, cfg.Token, tr.URL)
	}
	if tr.sawGitHub.Load() {
		t.Fatal("the GitHub token was used as an ingest token")
	}
	if strings.Contains(out, colleagueGitHubToken) {
		t.Fatal("enroll printed the GitHub token")
	}
}

// A re-run installer must not mint a token per run, and a revoked token must
// not stop it enrolling again.
func TestEnrollAgainReplacesATokenOnlyWhenItNoLongerWorks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		saved     string
		live      bool
		wantToken string
		enrols    int32
	}{
		{"an enrolled token that works", schema.EnrolledTokenPrefix + "old", true, schema.EnrolledTokenPrefix + "old", 0},
		{"a revoked token", schema.EnrolledTokenPrefix + "old", false, schema.EnrolledTokenPrefix + "new", 1},
		// The shared token works, but is not this machine's own.
		{"the shared token", "shared", true, schema.EnrolledTokenPrefix + "new", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fakeGH(t)
			var live []string
			if tc.live {
				live = append(live, tc.saved)
			}
			tr := newTracker(t, schema.EnrolledTokenPrefix+"new", live...)
			dir := t.TempDir()
			if err := config.Save(dir, config.Config{ServerURL: tr.URL, Token: tc.saved}); err != nil {
				t.Fatal(err)
			}

			if _, err := agentRun(t, "enroll", "-data", dir, "-server", tr.URL); err != nil {
				t.Fatalf("enroll: %v", err)
			}
			cfg, _ := config.Load(dir)
			if cfg.Token != tc.wantToken || tr.enrolled.Load() != tc.enrols {
				t.Fatalf("token %q after %d enrolments; want %q after %d",
					cfg.Token, tr.enrolled.Load(), tc.wantToken, tc.enrols)
			}
		})
	}
}

func TestEnrollRefusesToSendTheGitHubTokenInTheClear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No gh at all: the address is refused before gh is asked for anything.
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()

	_, err := agentRun(t, "enroll", "-data", dir, "-server", "http://tracker.example.com")
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("enroll over plain http: %v", err)
	}
	if _, serr := os.Stat(config.Path(dir)); serr == nil {
		t.Fatal("a refused enrolment wrote a config")
	}
}

// Not signed in, or a gh too old for `auth token`: gh's own words say which.
func TestEnrollPassesOnWhyGHHasNoToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'no oauth token found for github.com' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	tr := newTracker(t, schema.EnrolledTokenPrefix+"issued")

	_, err := agentRun(t, "enroll", "-data", t.TempDir(), "-server", tr.URL)
	if err == nil || !strings.Contains(err.Error(), "no oauth token found") ||
		!strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("enroll with gh signed out: %v", err)
	}
	if tr.enrolled.Load() != 0 {
		t.Fatal("enrolled without a GitHub token")
	}
}

func TestEnrollSaysHowToGetGitHubCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	tr := newTracker(t, schema.EnrolledTokenPrefix+"issued")

	_, err := agentRun(t, "enroll", "-data", t.TempDir(), "-server", tr.URL)
	if err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("enroll without gh: %v", err)
	}
}
