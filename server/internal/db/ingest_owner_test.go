package db

import (
	"context"
	"errors"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

func email(t *testing.T, d *DB, ref string) string {
	t.Helper()
	var e string
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT email FROM account WHERE ref = ?`, ref).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// A machine or account stored before owners were recorded has none, and is
// claimed by the next upload that names it.
func TestARowFromBeforeOwnersIsClaimedByTheNextUpload(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	if _, err := d.write.ExecContext(ctx, `
		INSERT INTO machine (id, hostname, agent_version, first_seen, last_seen) VALUES ('m', 'h', '', 0, 0);
		INSERT INTO account (ref, email, first_seen, last_seen) VALUES ('anthropic:a', 'dev@example.com', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	ingest(t, d, ev("a", 10, schema.CostBilled))

	_, err := d.Ingest(ctx, "other", &schema.Batch{V: schema.Version, MachineID: "m"})
	var claimed *MachineClaimedError
	if !errors.As(err, &claimed) || claimed.Owner != testLogin {
		t.Fatalf("another login's upload from the claimed machine: %v, want it refused as %s's", err, testLogin)
	}
	if _, err := d.Ingest(ctx, "other", &schema.Batch{V: schema.Version, MachineID: "m2",
		Accounts: []schema.Account{{Ref: "anthropic:a", Email: "other@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if got := email(t, d, "anthropic:a"); got != "dev@example.com" {
		t.Fatalf("email = %q after another login reported the claimed account", got)
	}
}

// The person the dashboard shows is the email, so an email another login's
// account carries would move usage onto that colleague: it is never written
// to an account of another login, new or old.
func TestAnEmailAnotherLoginHoldsIsNeverTaken(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	report := func(login, machine string, accounts ...schema.Account) {
		t.Helper()
		if _, err := d.Ingest(ctx, login, &schema.Batch{
			V: schema.Version, MachineID: machine, Accounts: accounts}); err != nil {
			t.Fatal(err)
		}
	}
	report("alice", "m1", schema.Account{Ref: "anthropic:alice", Email: "alice@example.com"})
	report("bob", "m2", schema.Account{Ref: "anthropic:bob", Email: "bob@example.com"},
		schema.Account{Ref: "anthropic:new", Email: "Alice@Example.com"})
	report("bob", "m2", schema.Account{Ref: "anthropic:bob", Email: "alice@example.com"})
	report("alice", "m1", schema.Account{Ref: "openai:alice", Email: "alice@example.com"})

	for ref, want := range map[string]string{
		"anthropic:new":   "",                  // bob's new account
		"anthropic:bob":   "bob@example.com",   // bob's own, re-pointed
		"openai:alice":    "alice@example.com", // alice's second login
		"anthropic:alice": "alice@example.com",
	} {
		if got := email(t, d, ref); got != want {
			t.Errorf("%s has email %q, want %q", ref, got, want)
		}
	}
}

// The agent checks its token with a batch that names no machine and carries
// nothing; rows with no machine to be stored under are refused.
func TestOnlyABatchWithNothingToStoreMayOmitItsMachine(t *testing.T) {
	d, ctx := newDB(t), context.Background()
	if _, err := d.Ingest(ctx, testLogin, &schema.Batch{V: schema.Version}); err != nil {
		t.Fatalf("an empty batch with no machine: %v", err)
	}
	for name, b := range map[string]schema.Batch{
		"events":          {Events: []schema.Event{ev("a", 10, schema.CostBilled)}},
		"unknown sources": {UnknownSource: []schema.UnknownSource{{Path: "/p"}}},
	} {
		if _, err := d.Ingest(ctx, testLogin, &b); !errors.Is(err, ErrNoMachine) {
			t.Errorf("%s with no machine: %v, want ErrNoMachine", name, err)
		}
	}
	if n := rawCount(t, d); n != 0 {
		t.Fatalf("%d events stored with no machine", n)
	}
}
