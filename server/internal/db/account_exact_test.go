package db

import (
	"context"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// A request CLIProxyAPI routed names the account that served it. That
// replaces the signed-in account stamped on an earlier reading, and nothing
// inferred ever moves it back: an inferred account is whoever is signed in
// when a file is read, which a re-read after a switch gets wrong.
func TestAnExactAccountReplacesAnInferredOneAndNeverTheReverse(t *testing.T) {
	inferred := func(ref string) schema.Event {
		e := ev("r", 100, schema.CostRateCard)
		e.AccountRef = ref
		return e
	}
	exact := func(ref string) schema.Event {
		e := inferred(ref)
		e.AccountExact = true
		return e
	}
	for _, tc := range []struct {
		name      string
		readings  []schema.Event
		wantRef   string
		wantExact bool
	}{
		{"exact after inferred", []schema.Event{inferred("anthropic:a"), exact("anthropic:b")}, "anthropic:b", true},
		{"inferred after exact", []schema.Event{exact("anthropic:b"), inferred("anthropic:a")}, "anthropic:b", true},
		{"inferred after inferred", []schema.Event{inferred("anthropic:a"), inferred("anthropic:c")}, "anthropic:a", false},
		{"exact after exact", []schema.Event{exact("anthropic:b"), exact("anthropic:a")}, "anthropic:a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDB(t)
			for _, e := range tc.readings {
				ingest(t, d, e)
			}
			ref, isExact := accountOf(t, d, "r")
			if ref != tc.wantRef || isExact != tc.wantExact {
				t.Fatalf("stored %s (exact %v), want %s (exact %v)", ref, isExact, tc.wantRef, tc.wantExact)
			}
		})
	}
}

// The move must reach the dashboard's per-person totals, which read
// event_day: its update trigger re-keys the day's sums onto the new account.
func TestAnExactAccountMovesThePersonsTotals(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, ev("r", 100, schema.CostRateCard))

	e := ev("r", 100, schema.CostRateCard)
	e.AccountRef, e.AccountExact = "anthropic:b", true
	if _, err := d.Ingest(ctx, testLogin, &schema.Batch{
		V: schema.Version, MachineID: "m", Events: []schema.Event{e},
		Accounts: []schema.Account{
			{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"},
			{Ref: "anthropic:b", Provider: "anthropic", Email: "second@example.com"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	groups, err := d.Breakdown(ctx, Window{From: "2000-01-01"}, "person")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Key != "second@example.com" {
		t.Fatalf("got %+v, want the request's tokens on the account that served it", groups)
	}
}

// An exact account another login holds is not taken: the event is credited to
// the machine's account, as any event naming that account is, and it is no
// longer the account that served the request, so it is not marked exact.
func TestAnExactAccountAnotherLoginHoldsIsNotTaken(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	if _, err := d.Ingest(ctx, "someone-else", &schema.Batch{
		V: schema.Version, MachineID: "theirs",
		Accounts: []schema.Account{{Ref: "anthropic:b", Provider: "anthropic", Email: "them@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	e := ev("r", 100, schema.CostRateCard)
	e.AccountRef, e.AccountExact = "anthropic:b", true
	ingest(t, d, e)

	if ref, isExact := accountOf(t, d, "r"); ref != "anthropic:a" || isExact {
		t.Fatalf("stored %s (exact %v), want the machine's anthropic:a, not exact", ref, isExact)
	}
}

func accountOf(t *testing.T, d *DB, id string) (string, bool) {
	t.Helper()
	var ref string
	var isExact bool
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT account_ref, account_exact FROM event WHERE id = ?`, id).Scan(&ref, &isExact); err != nil {
		t.Fatal(err)
	}
	return ref, isExact
}
