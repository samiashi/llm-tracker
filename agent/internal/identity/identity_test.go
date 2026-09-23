package identity

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// home points HOME at a directory holding files, keyed by their path under it.
func home(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// idToken is an unsigned JWT carrying claims.
func idToken(claims string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
}

const claudeSignedIn = `{"oauthAccount":{"accountUuid":"0a1b2c3d-0000-4000-8000-00000000abcd",` +
	`"emailAddress":"dev@example.com","billingType":"stripe_subscription"}}`

// A sign-out must reach the account timeline, or the account signed in
// before is credited with everything after it; a config that cannot be read
// must not, or a half-written ~/.claude.json signs the seat out.
func TestLoginsTellASignOutFromAConfigThatCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  map[string]string // provider -> ref; absent means not reported
	}{
		{"signed in to both", map[string]string{
			".claude.json":     claudeSignedIn,
			".codex/auth.json": `{"tokens":{"account_id":"acct_1","id_token":""}}`,
		}, map[string]string{"anthropic": "anthropic:0a1b2c3d-0000-4000-8000-00000000abcd", "openai": "openai:acct_1"}},
		{"signed out of Claude Code, Codex on an API key", map[string]string{
			".claude.json":     `{"numStartups":3}`,
			".codex/auth.json": `{"OPENAI_API_KEY":"sk-test","tokens":null}`,
		}, map[string]string{"anthropic": "", "openai": ""}},
		{"no config at all: Codex deletes auth.json on logout", nil,
			map[string]string{"anthropic": "", "openai": ""}},
		{"half-written configs", map[string]string{
			".claude.json":     `{"oauthAccount":{"accountUu`,
			".codex/auth.json": ``,
		}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home(t, tc.files)
			got := map[string]string{}
			for _, l := range Logins() {
				ref := ""
				if l.Account != nil {
					ref = l.Account.Ref
				}
				got[l.Provider] = ref
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Logins reported %v, want %v", got, tc.want)
			}
			for p, ref := range tc.want {
				if r, ok := got[p]; !ok || r != ref {
					t.Fatalf("Logins reported %v, want %v", got, tc.want)
				}
			}
		})
	}

	t.Run("a config that cannot be read", func(t *testing.T) {
		dir := home(t, map[string]string{".claude.json": claudeSignedIn})
		if err := os.Chmod(filepath.Join(dir, ".claude.json"), 0); err != nil {
			t.Fatal(err)
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode")
		}
		for _, l := range Logins() {
			if l.Provider == "anthropic" {
				t.Fatalf("an unreadable ~/.claude.json was reported as %+v", l)
			}
		}
	})
}

// A ref joins an event to the email that names its owner. Cowork paths and
// Claude Code's Cowork sessions stamp "anthropic:<uuid>", so the signed-in
// account must read the same, or one colleague becomes two.
func TestTheClaudeAccountIsNamedTheWayCoworkPathsAre(t *testing.T) {
	dir := home(t, map[string]string{".claude.json": claudeSignedIn})
	a, err := claudeAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := schema.Account{Ref: "anthropic:0a1b2c3d-0000-4000-8000-00000000abcd", Provider: "anthropic",
		Email: "dev@example.com", PlanType: "stripe_subscription"}
	if a == nil || *a != want {
		t.Fatalf("claudeAccount = %+v, want %+v", a, want)
	}
}

// The id_token is base64url: a claim whose encoding carries '-' or '_' must
// still decode, or the email and plan silently go missing.
func TestTheCodexAccountReadsTheIDTokensClaims(t *testing.T) {
	claims := `{"email":"dev@example.com","https://api.openai.com/auth":{"chatgpt_plan_type":"pro"},"n":"???>>>"}`
	if tok := idToken(claims); !strings.ContainsAny(strings.Split(tok, ".")[1], "-_") {
		t.Fatalf("setup: %s exercises nothing base64url-specific", tok)
	}
	dir := home(t, map[string]string{".codex/auth.json": `{"tokens":{"account_id":"acct_1",` +
		`"id_token":"` + idToken(claims) + `"}}`})
	a, err := codexAccount(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := schema.Account{Ref: "openai:acct_1", Provider: "openai", Email: "dev@example.com", PlanType: "pro"}
	if a == nil || *a != want {
		t.Fatalf("codexAccount = %+v, want %+v", a, want)
	}
}

// The server needs a stable handle, not the Mac's platform UUID.
func TestTheMachineIDNeverCarriesTheHardwareUUID(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("reads the Mac's platform UUID")
	}
	out, err := exec.CommandContext(context.Background(), "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		t.Skip("ioreg unavailable:", err)
	}
	m := platformUUID.FindSubmatch(out)
	if len(m) < 2 {
		t.Skip("no IOPlatformUUID")
	}
	id := hardwareID(context.Background())
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 ||
		strings.Contains(strings.ToLower(id), strings.ToLower(strings.ReplaceAll(string(m[1]), "-", ""))) {
		t.Fatalf("machine id %q: want 32 hex characters derived from, not carrying, the hardware UUID", id)
	}
}

// Every member of a ChatGPT workspace shares its account id. Keyed on that
// alone, their Codex usage lands on one ref, which the server credits to
// whichever of them uploaded last.
func TestCodexAccountsInOneWorkspaceStayApart(t *testing.T) {
	ref := func(claims string) string {
		t.Helper()
		dir := home(t, map[string]string{".codex/auth.json": `{"tokens":{"account_id":"ws_1",` +
			`"id_token":"` + idToken(claims) + `"}}`})
		a, err := codexAccount(dir)
		if err != nil || a == nil {
			t.Fatalf("codexAccount = %+v, %v", a, err)
		}
		return a.Ref
	}
	alice := ref(`{"email":"alice@example.com","https://api.openai.com/auth":{"chatgpt_user_id":"user-alice"}}`)
	bob := ref(`{"email":"bob@example.com","https://api.openai.com/auth":{"chatgpt_user_id":"user-bob"}}`)
	if alice != "openai:ws_1:user-alice" || bob != "openai:ws_1:user-bob" {
		t.Fatalf("refs %q and %q: want each member's own", alice, bob)
	}
	if got := ref(`{"email":"carol@example.com"}`); got != "openai:ws_1" {
		t.Fatalf("with no user id in the token: %q, want the account id alone", got)
	}
}
