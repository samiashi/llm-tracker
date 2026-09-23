// Package identity answers which machine this is, and which account was active
// when an event was captured.
//
// The second has a deadline: ~/.codex/auth.json holds one account_id, which
// switching accounts overwrites in place, so the account behind old sessions
// must be recorded at capture time or it is lost.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/samiashi/llm-tracker/schema"
)

type metaStore interface {
	Meta(ctx context.Context, k string) (string, error)
	SetMeta(ctx context.Context, k, v string) error
}

// MachineID returns a stable per-machine identifier.
//
// It prefers the hardware UUID so the identity survives a deleted or relocated
// store: a generated UUID changes with every fresh store and leaves phantom
// machines on the server. The hardware value is hashed: the server needs a
// stable handle, not a serial number that identifies the hardware.
func MachineID(ctx context.Context, s metaStore) (string, error) {
	if v, err := s.Meta(ctx, "machine_id"); err != nil {
		return "", err
	} else if v != "" {
		return v, nil
	}
	id := hardwareID(ctx)
	if id == "" {
		id = uuid.NewString()
	}
	return id, s.SetMeta(ctx, "machine_id", id)
}

// hardwareID derives a stable identifier from the Mac's platform UUID,
// returning "" when it cannot be read.
func hardwareID(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	m := platformUUID.FindSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	sum := sha256.Sum256(append([]byte("llm-tracker-machine:"), m[1]...))
	return hex.EncodeToString(sum[:16])
}

var platformUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// Login is one provider's sign-in state, as its harness's config records it.
type Login struct {
	Provider string
	// Account is who is signed in, or nil when the config names nobody:
	// signed out, or on an API key.
	Account *schema.Account
}

// Logins returns the sign-in state of every provider whose config could be
// read, Anthropic first. A missing config means nobody is signed in: Codex
// deletes auth.json on logout. One that exists but cannot be read or parsed
// is left out rather than taken for a sign-out: ~/.claude.json is rewritten
// constantly, and a half-written copy would end the account's window and
// strip its seat from the usage that follows.
func Logins() []Login {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []Login
	for _, p := range []struct {
		provider string
		read     func(home string) (*schema.Account, error)
	}{
		{"anthropic", claudeAccount},
		{"openai", codexAccount},
	} {
		if a, err := p.read(home); err == nil {
			out = append(out, Login{Provider: p.provider, Account: a})
		}
	}
	return out
}

// All returns the accounts signed in now, Anthropic first.
func All() []*schema.Account {
	var out []*schema.Account
	for _, l := range Logins() {
		if l.Account != nil {
			out = append(out, l.Account)
		}
	}
	return out
}

// readConfig returns a harness's config file, or nil when there is none.
func readConfig(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// claudeAccount reads the signed-in Anthropic account from ~/.claude.json,
// not the Keychain: reading that triggers a consent prompt.
func claudeAccount(home string) (*schema.Account, error) {
	b, err := readConfig(filepath.Join(home, ".claude.json"))
	if b == nil || err != nil {
		return nil, err
	}
	var doc struct {
		OAuthAccount struct {
			AccountUUID  string `json:"accountUuid"`
			EmailAddress string `json:"emailAddress"`
			BillingType  string `json:"billingType"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	a := doc.OAuthAccount
	if a.AccountUUID == "" {
		return nil, nil
	}
	return &schema.Account{
		Ref:      "anthropic:" + a.AccountUUID,
		Provider: "anthropic",
		Email:    a.EmailAddress,
		PlanType: a.BillingType,
	}, nil
}

// codexAccount reads the signed-in OpenAI account from ~/.codex/auth.json.
//
// The plan type lives in the id_token's claims, so the JWT payload is decoded.
// The signature is never verified and the token never sent: it only names the
// account that is logged in.
func codexAccount(home string) (*schema.Account, error) {
	b, err := readConfig(filepath.Join(home, ".codex", "auth.json"))
	if b == nil || err != nil {
		return nil, err
	}
	var doc struct {
		Tokens struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Tokens.AccountID == "" {
		return nil, nil
	}
	acct := &schema.Account{
		Ref:      "openai:" + doc.Tokens.AccountID,
		Provider: "openai",
	}
	if claims := decodeJWTClaims(doc.Tokens.IDToken); claims != nil {
		if v, ok := claims["email"].(string); ok {
			acct.Email = v
		}
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if v, ok := auth["chatgpt_plan_type"].(string); ok {
				acct.PlanType = v
			}
		}
	}
	return acct, nil
}

// decodeJWTClaims returns the payload of a JWT without verifying it. Returns
// nil on anything malformed rather than failing the whole collection pass.
func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}
