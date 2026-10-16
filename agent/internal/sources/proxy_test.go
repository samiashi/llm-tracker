package sources

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/identity"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

const (
	signedInUUID = "0a1b2c3d-0000-4000-8000-00000000000a"
	secondUUID   = "0a1b2c3d-0000-4000-8000-00000000000b"
)

// proxyHome lays out what CLIProxyAPI and its usage monitor leave on disk:
// an auth file per account and the monitor's events table, as it creates them.
func proxyHome(t *testing.T) (home string, monitor *sql.DB) {
	t.Helper()
	home = t.TempDir()
	auth := filepath.Join(home, identity.ProxyAuthDir)
	if err := os.MkdirAll(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"claude-1-first@example.com.json": `{"type":"claude","email":"first@example.com",` +
			`"account_uuid":"` + signedInUUID + `","access_token":"secret"}`,
		"claude-2-second@example.com.json": `{"type":"claude","email":"second@example.com",` +
			`"account_uuid":"` + secondUUID + `","access_token":"secret"}`,
		"codex-3.json": `{"type":"codex","email":"third@example.com","account_uuid":"x"}`,
	} {
		if err := os.WriteFile(filepath.Join(auth, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, ProxyLedgerPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;
		CREATE TABLE events(
		  id TEXT PRIMARY KEY, timestamp TEXT NOT NULL, email TEXT NOT NULL,
		  model TEXT, input_tokens INTEGER, output_tokens INTEGER,
		  cache_read_tokens INTEGER, cache_creation_tokens INTEGER,
		  total_tokens INTEGER, failed INTEGER, status_code INTEGER, latency_ms INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return home, db
}

// record adds one proxied request, as the monitor writes it.
func record(t *testing.T, db *sql.DB, id string, at time.Time, email string, failed bool,
	model string, input, cacheRead, cacheWrite int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO events VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, at.UTC().Format("2006-01-02T15:04:05.000000+00:00"), email, model,
		input, 99, cacheRead, cacheWrite, input+99+cacheRead+cacheWrite,
		failed, 200, 1000); err != nil {
		t.Fatal(err)
	}
}

func proxied(at time.Time, input, cacheRead, cacheWrite int64) schema.Event {
	return schema.Event{
		Source: schema.SourceClaudeCode, Model: "claude-opus-5", TS: at,
		Usage: schema.Usage{InputTokens: input, CacheReadTokens: cacheRead, CacheWrite1hTokens: cacheWrite},
	}
}

func openLedger(t *testing.T, home string) *ProxyLedger {
	t.Helper()
	l, err := OpenProxyLedger(home)
	if err != nil || l == nil {
		t.Fatalf("OpenProxyLedger = %v, %v", l, err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// A record starts when its request does and the transcript line is written
// when the reply ends, so the record comes first, and of several identical
// requests the latest to precede the event is the one that served it.
func TestAProxiedRequestTakesTheAccountOfTheRecordThatServedIt(t *testing.T) {
	end := time.Date(2026, 10, 16, 9, 0, 30, 0, time.UTC) // records straddle the hour
	home, db := proxyHome(t)
	record(t, db, "earlier", end.Add(-5*time.Minute), "first@example.com", false, "claude-opus-5", 2, 500, 70)
	record(t, db, "served", end.Add(-40*time.Second), "Second@example.com", false, "claude-opus-5", 2, 500, 70)
	record(t, db, "next", end.Add(time.Minute), "first@example.com", false, "claude-opus-5", 2, 500, 70)
	record(t, db, "failed", end.Add(-time.Second), "first@example.com", true, "claude-opus-5", 2, 500, 70)
	record(t, db, "unnamed", end.Add(-time.Minute), "gone@example.com", false, "claude-opus-5", 9, 9, 9)
	l := openLedger(t, home)

	for _, tc := range []struct {
		name string
		e    schema.Event
		want string
	}{
		{"latest preceding record", proxied(end, 2, 500, 70), "anthropic:" + secondUUID},
		{"the earlier request's reply", proxied(end.Add(-4*time.Minute), 2, 500, 70), "anthropic:" + signedInUUID},
		{"other counts", proxied(end, 2, 501, 70), ""},
		{"other model", func() schema.Event { e := proxied(end, 2, 500, 70); e.Model = "claude-haiku-5"; return e }(), ""},
		{"no record started before it", proxied(end.Add(-20*time.Minute), 2, 500, 70), ""},
		{"an account the proxy no longer holds", proxied(end, 9, 9, 9), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := l.Account(context.Background(), tc.e)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("Account = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestWithoutTheProxyNothingIsMatched(t *testing.T) {
	l, err := OpenProxyLedger(t.TempDir())
	if err != nil || l != nil {
		t.Fatalf("OpenProxyLedger = %v, %v; want nil, nil", l, err)
	}
	if _, ok := l.Account(context.Background(), proxied(time.Now(), 1, 1, 1)); ok {
		t.Fatal("a nil ledger matched")
	}
}

// Read through the adapter, a proxied response and the attempt its fallback
// abandoned both go to the proxy's account, at rate card; a request the proxy
// never saw keeps the account signed in at the time.
func TestClaudeCodeCreditsAProxiedRequestToTheAccountThatServedIt(t *testing.T) {
	end := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	home, db := proxyHome(t)
	record(t, db, "a", end.Add(-30*time.Second), "second@example.com", false, "claude-opus-5", 2, 500, 70)
	transcript(t, home, end,
		`{"input_tokens":2,"output_tokens":9,"cache_read_input_tokens":500,"cache_creation_input_tokens":70,`+
			`"cache_creation":{"ephemeral_1h_input_tokens":70},"iterations":[`+
			`{"model":"claude-sonnet-5","input_tokens":40,"output_tokens":3},`+
			`{"model":"claude-opus-5","input_tokens":2,"output_tokens":9}]}`,
		`{"input_tokens":7,"output_tokens":9}`)

	c, st := proxyCtx(t, home)
	if _, err := (ClaudeCode{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	checkAccounts(t, storedEvents(t, st), map[string]string{
		"req_1|msg_1":   "anthropic:" + secondUUID,
		"req_1|msg_1#0": "anthropic:" + secondUUID,
		"req_2|msg_2":   "anthropic:" + signedInUUID,
	})
}

// The monitor takes a record a few seconds after the request ends, and can be
// an hour behind, so a pass can store an event before its record exists. A
// later pass finds it and requeues the row.
func TestAnEventStoredBeforeItsRecordTakesTheAccountOnALaterPass(t *testing.T) {
	end := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	home, db := proxyHome(t)
	transcript(t, home, end,
		`{"input_tokens":2,"output_tokens":9,"cache_read_input_tokens":500,"cache_creation_input_tokens":70,`+
			`"iterations":[{"model":"claude-sonnet-5","input_tokens":40,"output_tokens":3},`+
			`{"model":"claude-opus-5","input_tokens":2,"output_tokens":9}]}`,
		`{"input_tokens":7,"output_tokens":9}`)
	c, st := proxyCtx(t, home)
	if _, err := (ClaudeCode{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ids, _, _ := st.Unsent(context.Background(), 10)
	if err := st.MarkSent(context.Background(), ids); err != nil {
		t.Fatal(err)
	}

	record(t, db, "a", end.Add(-30*time.Second), "second@example.com", false, "claude-opus-5", 2, 500, 70)
	c, _ = proxyCtxOn(t, home, st)
	n, err := ReattributeProxied(context.Background(), c, time.Now().Add(-ProxyRecheck))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("moved %d rows, want the response and its abandoned attempt", n)
	}
	// Queued for upload again: only the two that moved.
	checkAccounts(t, storedEvents(t, st), map[string]string{
		"req_1|msg_1":   "anthropic:" + secondUUID,
		"req_1|msg_1#0": "anthropic:" + secondUUID,
	})
	if n, err := ReattributeProxied(context.Background(), c, time.Now().Add(-ProxyRecheck)); err != nil || n != 0 {
		t.Fatalf("a second sweep moved %d rows (%v), want none", n, err)
	}
}

// transcript writes two responses ending at end: req_1 with usage1, and req_2
// with usage2, which the proxy never saw.
func transcript(t *testing.T, home string, end time.Time, usage1, usage2 string) {
	t.Helper()
	dir := filepath.Join(home, ".claude/projects/-Users-dev-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(n, usage string) string {
		return `{"type":"assistant","requestId":"req_` + n + `","sessionId":"s1","timestamp":"` +
			end.Format(time.RFC3339Nano) + `","message":{"id":"msg_` + n +
			`","model":"claude-opus-5","usage":` + usage + `}}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(line("1", usage1)+line("2", usage2)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func proxyCtx(t *testing.T, home string) (*Ctx, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return proxyCtxOn(t, home, st)
}

func proxyCtxOn(t *testing.T, home string, st *store.Store) (*Ctx, *store.Store) {
	t.Helper()
	return &Ctx{Store: st, MachineID: "m", Home: home, Proxy: openLedger(t, home),
		Accounts: map[string]*schema.Account{
			"anthropic": {Ref: "anthropic:" + signedInUUID, Provider: "anthropic"},
		}}, st
}

func checkAccounts(t *testing.T, evs []schema.Event, want map[string]string) {
	t.Helper()
	if len(evs) != len(want) {
		t.Fatalf("stored %d events, want %d", len(evs), len(want))
	}
	for _, e := range evs {
		ref, ok := want[e.NativeID]
		if !ok {
			t.Fatalf("unexpected event %s", e.NativeID)
		}
		exact := ref != "anthropic:"+signedInUUID
		if e.AccountRef != ref || e.AccountExact != exact || e.CostBasis != schema.CostRateCard {
			t.Errorf("%s: account %s (exact %v, %s), want %s (exact %v, rate card)",
				e.NativeID, e.AccountRef, e.AccountExact, e.CostBasis, ref, exact)
		}
	}
}
