package db

import (
	"context"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

func TestAnIssuedTokenAuthenticatesUntilItsLoginIsRevoked(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	laptop, err := d.IssueToken(ctx, "alice", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	desktop, err := d.IssueToken(ctx, "alice", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.IssueToken(ctx, "bob", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(laptop, schema.EnrolledTokenPrefix) || laptop == desktop {
		t.Fatalf("tokens %q and %q: want distinct, prefixed values", laptop, desktop)
	}

	for _, tok := range []string{laptop, desktop} {
		if login, ok, err := d.TokenLogin(ctx, tok); err != nil || !ok || login != "alice" {
			t.Fatalf("TokenLogin = %q, %v, %v; want alice", login, ok, err)
		}
	}

	// A login differing only in case is the same GitHub account.
	n, err := d.RevokeTokens(ctx, "Alice")
	if err != nil || n != 2 {
		t.Fatalf("RevokeTokens = %d, %v; want both of alice's tokens", n, err)
	}
	for _, tok := range []string{laptop, desktop} {
		if _, ok, err := d.TokenLogin(ctx, tok); err != nil || ok {
			t.Fatalf("a revoked token still authenticates (err %v)", err)
		}
	}
	if _, ok, _ := d.TokenLogin(ctx, other); !ok {
		t.Fatal("revoking alice revoked bob's token too")
	}
	if n, _ := d.RevokeTokens(ctx, "alice"); n != 0 {
		t.Fatalf("revoking again counted %d tokens; only live ones count", n)
	}
}

func TestATokenThatWasNeverIssuedIsRefused(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	if _, err := d.IssueToken(ctx, "alice", "laptop"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", schema.EnrolledTokenPrefix, schema.EnrolledTokenPrefix + "made-up"} {
		if _, ok, err := d.TokenLogin(ctx, tok); err != nil || ok {
			t.Fatalf("TokenLogin(%q) = %v, %v; want refused", tok, ok, err)
		}
	}
}

// The database is backed up and snapshotted, so it must hold nothing that
// works as a credential.
func TestOnlyATokensHashIsStored(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	tok, err := d.IssueToken(ctx, "alice", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := d.read.QueryRowContext(ctx, `SELECT hash FROM agent_token`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != tokenHash(tok) || strings.Contains(stored, strings.TrimPrefix(tok, schema.EnrolledTokenPrefix)) {
		t.Fatalf("stored %q for token %q; want its SHA-256 and nothing of the token", stored, tok)
	}
}
