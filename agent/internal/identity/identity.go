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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Account identifies one provider login on this machine.
//
// Email is carried deliberately: per-person attribution is the point of a team
// dashboard. It is not the only personal data sent -- the hostname, project
// paths, and harness paths that name the home directory go too; schema.Batch
// is the complete list.
type Account struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
}

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

// DetectClaude reads the active Anthropic account from ~/.claude.json, not the
// Keychain: reading that triggers a consent prompt.
func DetectClaude() (*Account, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
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
	return &Account{
		Ref:      "anthropic:" + a.AccountUUID,
		Provider: "anthropic",
		Email:    a.EmailAddress,
		PlanType: a.BillingType,
	}, nil
}

// DetectCodex reads the active OpenAI account from ~/.codex/auth.json.
//
// The plan type lives in the id_token's claims, so the JWT payload is decoded.
// The signature is never verified and the token never sent: it only names the
// account that is logged in.
func DetectCodex() (*Account, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
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
	acct := &Account{
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

// All returns every account detectable on this machine. A missing or
// unreadable config is not an error: people have one harness and not another.
func All() []*Account {
	var out []*Account
	if a, err := DetectClaude(); err == nil && a != nil {
		out = append(out, a)
	}
	if a, err := DetectCodex(); err == nil && a != nil {
		out = append(out, a)
	}
	return out
}
