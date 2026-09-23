package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// people ingests usage under every shape of person key ByPerson produces: an
// email two logins share, a login with no email, a ref with no account row,
// and events of no account at all.
func people(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	with := func(id, ref string, tok int64, daysAgo int) schema.Event {
		e := ev(id, tok, schema.CostBilled)
		e.AccountRef, e.TS = ref, time.Now().AddDate(0, 0, -daysAgo)
		return e
	}
	if _, err := d.Ingest(ctx, testLogin, &schema.Batch{V: schema.Version, MachineID: "m",
		Events: []schema.Event{
			with("work", "anthropic:a", 100, 40), with("codex", "openai:b", 200, 10),
			with("no-email", "anthropic:c", 400, 5), with("ghost", "ghost:x", 800, 3),
		},
		Accounts: []schema.Account{
			{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"},
			{Ref: "openai:b", Provider: "openai", Email: "dev@example.com"},
			{Ref: "anthropic:c", Provider: "anthropic"},
		}}); err != nil {
		t.Fatal(err)
	}
	// A batch with no accounts leaves its events with none to be credited to.
	orphan := with("orphan", "", 1_600, 1)
	orphan.MachineID = "m2"
	if _, err := d.Ingest(ctx, testLogin, &schema.Batch{V: schema.Version, MachineID: "m2",
		Events: []schema.Event{orphan}}); err != nil {
		t.Fatal(err)
	}
}

// The dashboard's By person rows are clickable, and each sends its key back as
// the filter: every key must select exactly the usage its row shows, or a
// click on it reports another total -- or, for "unknown", nothing at all.
func TestEveryPersonKeyFiltersToItsOwnTotals(t *testing.T) {
	d := newDB(t)
	people(t, d)
	ctx := context.Background()
	w := Window{From: "2000-01-01"}

	groups, err := d.Breakdown(ctx, w, "person")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(groups))
	for _, g := range groups {
		keys = append(keys, g.Key)
		w.Person = g.Key
		got, err := d.Totals(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		if got != g.Totals {
			t.Errorf("person %q: filtered totals %+v, but the breakdown row holds %+v", g.Key, got, g.Totals)
		}
	}
	if want := "unknown ghost:x anthropic:c dev@example.com"; strings.Join(keys, " ") != want {
		t.Fatalf("person keys %v, want %s", keys, want)
	}
}

// A person's history is their own: the prior period before their first day
// is unrecorded for them, however far back the team's goes.
func TestAPersonsHistoryIsTheirOwn(t *testing.T) {
	d := newDB(t)
	people(t, d)
	day := func(daysAgo int) string { return time.Now().AddDate(0, 0, -daysAgo).UTC().Format(time.DateOnly) }
	for _, c := range []struct{ person, first, last string }{
		{"", day(40), day(1)},
		{"dev@example.com", day(40), day(10)},
		{"anthropic:c", day(5), day(5)},
		{"unknown", day(1), day(1)},
		{"nobody@example.com", "", ""},
	} {
		first, last, err := d.DayRange(context.Background(), c.person)
		if err != nil || first != c.first || last != c.last {
			t.Errorf("history of %q: %q -> %q (%v), want %q -> %q", c.person, first, last, err, c.first, c.last)
		}
	}
}

// A person filter on raw events must range over the person's days in the
// window, not read their whole history to keep the window's rows.
func TestAPersonFilterReadsOnlyTheirDaysInTheWindow(t *testing.T) {
	d := newDB(t)
	where, args := Window{From: "2026-09-01", To: "2026-09-07", Person: "dev@example.com"}.where("")
	read := false
	for _, s := range explain(t, d, `SELECT SUM(total_tokens) FROM event WHERE `+where, args...) {
		if s.reads() != "event" {
			continue
		}
		read = true
		if !strings.Contains(s.detail, "(account_ref=? AND day>? AND day<?)") {
			t.Fatalf("event read as %q, not by account and day", s.detail)
		}
	}
	if !read {
		t.Fatal("the plan never reads event")
	}
}
