package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/db"
)

// uploadAs posts b as the agent holding token would.
func uploadAs(t *testing.T, s *Server, token string, b schema.Batch) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
	return rec
}

// enrolledAs issues an ingest token for login, as enrolment does.
func enrolledAs(t *testing.T, s *Server, login string) string {
	t.Helper()
	token, err := s.DB.IssueToken(context.Background(), login, login+"-laptop")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func usage(id, native string, source schema.Source, machine, ref string, collector int, tokens int64) schema.Event {
	return schema.Event{
		V: schema.Version, ID: id, NativeID: native, Source: source, TS: time.Now(),
		MachineID: machine, AccountRef: ref, Model: "claude-opus-5", Collector: collector,
		CostBasis: schema.CostRateCard, Usage: schema.Usage{InputTokens: tokens},
	}
}

// ownerView is what another login's token must not be able to move: alice's
// usage by person, her machine's row and its unknown-source list.
type ownerView struct {
	tokens            int64
	hostname, version string
	events            int64
	unknown           int
}

func viewOf(t *testing.T, s *Server, email, machine string) ownerView {
	t.Helper()
	ctx := context.Background()
	var v ownerView
	people, err := s.DB.Breakdown(ctx, db.Window{From: "2000-01-01"}.Normalise(), "person")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range people {
		if g.Key == email {
			v.tokens = g.Totals.TotalTokens
		}
	}
	agents, err := s.DB.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.MachineID == machine {
			v.hostname, v.version, v.events = a.Hostname, a.AgentVersion, a.Events
		}
	}
	unknown, err := s.DB.UnknownSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range unknown {
		if u.MachineID == machine {
			v.unknown++
		}
	}
	return v
}

// Every enrolled token can upload, so a batch's word for whose rows it
// carries is no proof: mallory's token, sending what her agent could, must
// leave alice's usage, email, machine and unknown sources exactly as they were.
func TestAnUploadCannotWriteAsAnotherLogin(t *testing.T) {
	s := newServer(t)
	alice, mallory := enrolledAs(t, s, "alice"), enrolledAs(t, s, "mallory")
	aliceAccounts := []schema.Account{
		{Ref: "anthropic:alice", Provider: "anthropic", Email: "alice@example.com"},
		{Ref: "openai:alice", Provider: "openai", Email: "alice@example.com"},
	}
	codexOld := func(n string, tokens int64) schema.Event {
		return usage(schema.MakeID(schema.SourceCodex, n), n, schema.SourceCodex,
			"m-alice", "openai:alice", 8, tokens)
	}
	rec := uploadAs(t, s, alice, schema.Batch{
		V: schema.Version, MachineID: "m-alice", Hostname: "alice-mbp", AgentVersion: "v1.4.0",
		Accounts: aliceAccounts,
		Events: []schema.Event{
			usage("e1", "req_1|msg_1", schema.SourceClaudeCode, "m-alice", "anthropic:alice", 9, 1_000),
			codexOld("rollout-a.jsonl#1", 400_000), codexOld("rollout-a.jsonl#2", 600_000),
		},
		UnknownSource: []schema.UnknownSource{{V: schema.Version, MachineID: "m-alice",
			Path: "/Users/alice/.amp", Hint: "Amp", SizeBytes: 10, Status: schema.StatusTodo}},
		UnknownComplete: true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's own upload: %d %s", rec.Code, rec.Body)
	}
	want := viewOf(t, s, "alice@example.com", "m-alice")
	if want.tokens != 1_001_000 || want.hostname != "alice-mbp" || want.unknown != 1 {
		t.Fatalf("setup: %+v", want)
	}

	mine := []schema.Account{{Ref: "anthropic:mallory", Provider: "anthropic", Email: "mallory@example.com"}}
	for _, tc := range []struct {
		name string
		b    schema.Batch
		code int
	}{
		{"a batch for alice's machine", schema.Batch{
			MachineID: "m-alice", Hostname: "renamed", AgentVersion: "v0.0.1", UnknownComplete: true,
			Events: []schema.Event{usage("f1", "f1", schema.SourceClaudeCode, "m-alice",
				"anthropic:alice", 9, 50_000_000)},
		}, http.StatusConflict},
		{"a new account carrying alice's email", schema.Batch{
			MachineID: "m-mallory",
			Accounts:  []schema.Account{{Ref: "anthropic:fake", Provider: "anthropic", Email: "alice@example.com"}},
			Events: []schema.Event{usage("f2", "f2", schema.SourceClaudeCode, "m-mallory",
				"anthropic:fake", 9, 50_000_000)},
		}, http.StatusOK},
		{"alice's account under another email", schema.Batch{
			MachineID: "m-mallory",
			Accounts:  []schema.Account{{Ref: "anthropic:alice", Provider: "anthropic", Email: "mallory@example.com"}},
		}, http.StatusOK},
		{"usage credited to alice's account", schema.Batch{
			MachineID: "m-mallory", Accounts: mine,
			Events: []schema.Event{usage("f3", "f3", schema.SourceClaudeCode, "m-mallory",
				"anthropic:alice", 9, 50_000_000)},
		}, http.StatusOK},
		{"rows filed under alice's machine", schema.Batch{
			MachineID: "m-mallory", Accounts: mine,
			Events: []schema.Event{usage("f4", "f4", schema.SourceClaudeCode, "m-alice",
				"anthropic:mallory", 9, 50_000_000)},
			UnknownSource: []schema.UnknownSource{{MachineID: "m-alice", Path: "/x", Hint: "X"}},
		}, http.StatusOK},
		// 00016 retires a machine's old-key Codex rows when it first reports
		// a content key, and refuses every old-key row it sends after that.
		{"a Codex re-key under alice's machine", schema.Batch{
			MachineID: "m-mallory", Accounts: mine,
			Events: []schema.Event{usage(schema.MakeID(schema.SourceCodex, "tc:forged"), "tc:forged",
				schema.SourceCodex, "m-alice", "openai:alice", 9, 1)},
		}, http.StatusOK},
		{"a longer, newer reading of alice's event", schema.Batch{
			MachineID: "m-mallory", Accounts: mine,
			Events: []schema.Event{usage("e1", "req_1|msg_1", schema.SourceClaudeCode, "m-mallory",
				"anthropic:mallory", 99, 50_000_000)},
		}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.b.V = schema.Version
			rec := uploadAs(t, s, mallory, tc.b)
			if rec.Code != tc.code {
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body, tc.code)
			}
			if got := viewOf(t, s, "alice@example.com", "m-alice"); got != want {
				t.Fatalf("alice's view moved from %+v to %+v", want, got)
			}
		})
	}

	// Her agent, still on the old Codex key, is not refused for the forgery.
	rec = uploadAs(t, s, alice, schema.Batch{V: schema.Version, MachineID: "m-alice",
		Hostname: "alice-mbp", AgentVersion: "v1.4.0", Accounts: aliceAccounts,
		Events: []schema.Event{codexOld("rollout-b.jsonl#1", 900_000)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's next upload: %d %s", rec.Code, rec.Body)
	}
	if got := viewOf(t, s, "alice@example.com", "m-alice").tokens; got != want.tokens+900_000 {
		t.Fatalf("alice holds %d tokens after her next upload, want %d", got, want.tokens+900_000)
	}
}

// The machine's owner is refused nothing, so a refusal names who holds it and
// how an admin releases it.
func TestARefusedMachineSaysHowToReleaseIt(t *testing.T) {
	s := newServer(t)
	alice, bob := enrolledAs(t, s, "alice"), enrolledAs(t, s, "bob")
	if rec := uploadAs(t, s, alice, schema.Batch{V: schema.Version, MachineID: "m1"}); rec.Code != http.StatusOK {
		t.Fatalf("alice: %d %s", rec.Code, rec.Body)
	}
	rec := uploadAs(t, s, bob, schema.Batch{V: schema.Version, MachineID: "m1"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "-revoke alice") {
		t.Fatalf("bob on alice's machine: %d %s, want 409 naming -revoke alice", rec.Code, rec.Body)
	}
}

// Rows are stored under their batch's machine, so a batch carrying some must
// name one; the agent's token check names none and carries nothing.
func TestRowsWithNoMachineAreABadRequest(t *testing.T) {
	s := newServer(t)
	if rec := uploadAs(t, s, testToken, schema.Batch{V: schema.Version}); rec.Code != http.StatusOK {
		t.Fatalf("the token check: %d %s", rec.Code, rec.Body)
	}
	rec := uploadAs(t, s, testToken, schema.Batch{V: schema.Version,
		Events: []schema.Event{event("e", "claude-opus-5", "anthropic:a", 10)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("events with no machine: %d %s, want 400", rec.Code, rec.Body)
	}
}

// The ownership rules must not get in the way of how colleagues actually
// work: several machines, re-enrolling one, and a laptop changing hands.
func TestALoginUploadsFromEveryMachineItEnrols(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	aliceAccount := []schema.Account{{Ref: "anthropic:alice", Provider: "anthropic", Email: "alice@example.com"}}
	upload := func(token, machine string, accounts []schema.Account, id string) int {
		t.Helper()
		ref := ""
		if len(accounts) > 0 {
			ref = accounts[0].Ref
		}
		return uploadAs(t, s, token, schema.Batch{V: schema.Version, MachineID: machine,
			Hostname: machine, Accounts: accounts,
			Events: []schema.Event{usage(id, id, schema.SourceClaudeCode, machine, ref, 9, 100)}}).Code
	}

	laptop, desktop := enrolledAs(t, s, "alice"), enrolledAs(t, s, "alice")
	if code := upload(laptop, "m1", aliceAccount, "a1"); code != http.StatusOK {
		t.Fatalf("alice's laptop: %d", code)
	}
	if code := upload(desktop, "m2", aliceAccount, "a2"); code != http.StatusOK {
		t.Fatalf("alice's desktop, on the same account: %d", code)
	}
	if code := upload(enrolledAs(t, s, "Alice"), "m1", aliceAccount, "a3"); code != http.StatusOK {
		t.Fatalf("alice's laptop re-enrolled, her login cased differently: %d", code)
	}
	if got := viewOf(t, s, "alice@example.com", "m1").tokens; got != 300 {
		t.Fatalf("alice holds %d tokens across her machines, want 300", got)
	}

	// The laptop goes to bob: refused until an admin revokes alice.
	bob := enrolledAs(t, s, "bob")
	bobAccount := []schema.Account{{Ref: "anthropic:bob", Provider: "anthropic", Email: "bob@example.com"}}
	if code := upload(bob, "m1", bobAccount, "b1"); code != http.StatusConflict {
		t.Fatalf("bob on alice's laptop before the revoke: %d, want 409", code)
	}
	if r, err := s.DB.RevokeTokens(ctx, "alice"); err != nil || r != (db.Revoked{Tokens: 3, Machines: 2, Accounts: 1}) {
		t.Fatalf("revoke = %+v, %v; want alice's 3 tokens, 2 machines and 1 account", r, err)
	}
	if code := upload(laptop, "m1", aliceAccount, "a4"); code != http.StatusUnauthorized {
		t.Fatalf("alice's revoked token: %d, want 401", code)
	}
	if code := upload(bob, "m1", bobAccount, "b1"); code != http.StatusOK {
		t.Fatalf("bob on the released laptop: %d", code)
	}
	if got := viewOf(t, s, "bob@example.com", "m1"); got.tokens != 100 || got.events != 3 {
		t.Fatalf("bob's view %+v, want his 100 tokens on a laptop holding 3 events", got)
	}
}
