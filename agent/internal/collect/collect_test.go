package collect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/sources"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// Run reads who is signed in from $HOME. Tests that call it point HOME at a
// directory of their own; this keeps any that forget off the developer's
// real ~/.claude.json and ~/.codex/auth.json.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "collect-test-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// The totals the daemon logs each pass.
func TestSummaryTotals(t *testing.T) {
	s := &Summary{PerSource: map[schema.Source]SourceSummary{
		schema.SourceClaudeCode: {Available: true, Files: 12, Found: 30, Stored: 4},
		schema.SourceCodex:      {Available: true, Files: 3, Found: 7, Stored: 7},
		// Installed and read, reporting nothing: a silent source.
		schema.SourceOpenCode: {Available: true, Files: 40, Found: 0, Stored: 0},
	}}

	if got := s.Found(); got != 37 {
		t.Errorf("Found() = %d, want 37", got)
	}
	if got := s.Stored(); got != 11 {
		t.Errorf("Stored() = %d, want 11", got)
	}
}

// Keyed on files matched, the alarm would fire on every caught-up source every
// pass, and nobody would read it.
func TestASourceIsSilentOnlyWhenItReadSomethingAndFoundNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    SourceSummary
		want bool
	}{
		{"caught up: matched files, read nothing",
			SourceSummary{Available: true, Files: 7}, false},
		{"read new data and parsed nothing from it",
			SourceSummary{Available: true, Files: 7, BytesRead: 4096}, true},
		{"read new data and parsed it",
			SourceSummary{Available: true, Files: 7, BytesRead: 4096, Found: 12}, false},
		{"read database rows and parsed nothing from them",
			SourceSummary{Available: true, Scanned: 40}, true},
		{"not installed",
			SourceSummary{Available: false}, false},
	} {
		if got := tc.s.Silent(); got != tc.want {
			t.Errorf("%s: Silent() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// row writes one event for a source as the given collector version would have.
func row(t *testing.T, st *store.Store, src schema.Source, id string, collector int) {
	t.Helper()
	e := schema.Event{
		V: schema.Version, ID: id, NativeID: id, Source: src,
		TS: time.Now(), MachineID: "m", Model: "m",
		Usage: schema.Usage{InputTokens: 1_000}, Collector: collector,
	}
	if _, err := st.CommitFile(context.Background(), "", 0, 0, []store.Record{{
		ID: id, TS: e.TS, TotalTokens: e.Usage.TotalTokens(), Collector: collector, Payload: e,
	}}, nil); err != nil {
		t.Fatal(err)
	}
}

// cursorFor writes a read position under one of a source's roots.
func cursorFor(t *testing.T, st *store.Store, src schema.Source, home string) string {
	t.Helper()
	ad, ok := sources.Lookup(string(src))
	if !ok {
		t.Fatalf("no adapter for %s", src)
	}
	path := sources.ScopeOf(ad, home).CursorPrefixes[0] + "session.jsonl"
	if _, err := st.CommitFile(context.Background(), path, 10, 10, nil, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func count(t *testing.T, st *store.Store) int64 {
	t.Helper()
	n, _, err := st.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func recorded(t *testing.T, st *store.Store) string {
	t.Helper()
	v, err := st.Meta(context.Background(), versionKey)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Such a store holds exactly the superseded rows; unpurged, they ship beside
// their re-read replacements and count every token twice.
func TestAStoreWithNoRecordedVersionIsStillPurged(t *testing.T) {
	st, _ := newStore(t)
	log := slog.New(slog.DiscardHandler)

	row(t, st, schema.SourceOpenCode, "opencode-session-row", 1)

	backfillIfUpgraded(context.Background(), st, t.TempDir(), log)

	if got := count(t, st); got != 0 {
		t.Errorf("%d superseded row(s) survived the upgrade", got)
	}
	if v := recorded(t, st); v != strconv.Itoa(sources.CollectorVersion) {
		t.Errorf("recorded version = %q, want %d", v, sources.CollectorVersion)
	}
}

// Run on every pass, it would delete what this build collected and re-read
// those sources from scratch each time.
func TestTheUpgradePassRunsOncePerUpgrade(t *testing.T) {
	st, _ := newStore(t)
	home := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	backfillIfUpgraded(context.Background(), st, home, log)

	// What this build then collects, for every source an upgrade purges or
	// re-reads.
	purged := sources.SourcesNeedingPurge(0)
	for _, src := range purged {
		row(t, st, src, string(src)+"-row", sources.CollectorVersion)
	}
	var cursors []string
	for _, src := range sources.SourcesNeedingBackfill(0) {
		cursors = append(cursors, cursorFor(t, st, src, home))
	}

	backfillIfUpgraded(context.Background(), st, home, log)

	if got := count(t, st); got != int64(len(purged)) {
		t.Errorf("%d of %d rows survived the next pass -- the upgrade purged them again",
			got, len(purged))
	}
	for _, p := range cursors {
		if off, _, _ := st.Cursor(context.Background(), p); off == 0 {
			t.Errorf("cursor %s was rewound again on the next pass", p)
		}
	}
}

// failWrites makes SQLite refuse every delete from table until the returned
// function is called, standing in for a disk or I/O error mid-upgrade.
func failWrites(t *testing.T, path, table string) (restore func()) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse BEFORE DELETE ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := db.Exec(`DROP TRIGGER refuse`); err != nil {
			t.Fatal(err)
		}
	}
}

// Recorded as done anyway, a failed upgrade leaves superseded rows beside their
// replacements, or stale rows never re-read.
func TestAnUpgradeThatFailsPartWayIsRetried(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	t.Run("purge", func(t *testing.T) {
		ctx := context.Background()
		st, path := newStore(t)
		row(t, st, schema.SourceOpenCode, "superseded", 1)
		restore := failWrites(t, path, "event")

		backfillIfUpgraded(ctx, st, t.TempDir(), log)
		if v := recorded(t, st); v != "" {
			t.Fatalf("version %q recorded although the purge failed", v)
		}
		if got := count(t, st); got != 1 {
			t.Fatalf("%d rows, want the superseded one still there to retry", got)
		}

		restore()
		backfillIfUpgraded(ctx, st, t.TempDir(), log)
		if got := count(t, st); got != 0 {
			t.Errorf("the retry left %d superseded row(s)", got)
		}
		if v := recorded(t, st); v != strconv.Itoa(sources.CollectorVersion) {
			t.Errorf("recorded version = %q after a clean retry, want %d", v, sources.CollectorVersion)
		}
	})

	t.Run("cursor reset", func(t *testing.T) {
		ctx := context.Background()
		st, path := newStore(t)
		home := t.TempDir()
		// A source the upgrade re-reads but does not purge, so the failure is
		// the reset's own.
		var src schema.Source
		purged := sources.SourcesNeedingPurge(0)
		for _, s := range sources.SourcesNeedingBackfill(0) {
			if !slices.Contains(purged, s) {
				src = s
				break
			}
		}
		if src == "" {
			t.Skip("every re-read source is also purged")
		}
		cursor := cursorFor(t, st, src, home)
		restore := failWrites(t, path, "cursor")

		backfillIfUpgraded(ctx, st, home, log)
		if v := recorded(t, st); v != "" {
			t.Fatalf("version %q recorded although the cursor reset failed", v)
		}

		restore()
		backfillIfUpgraded(ctx, st, home, log)
		if off, _, _ := st.Cursor(ctx, cursor); off != 0 {
			t.Errorf("the retry left %s's cursor at %d", src, off)
		}
		if v := recorded(t, st); v != strconv.Itoa(sources.CollectorVersion) {
			t.Errorf("recorded version = %q after a clean retry, want %d", v, sources.CollectorVersion)
		}
	})
}

func TestADowngradeIsRecordedAndDiscardsNothing(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	home := t.TempDir()

	if err := st.SetMeta(ctx, versionKey, strconv.Itoa(sources.CollectorVersion+2)); err != nil {
		t.Fatal(err)
	}
	row(t, st, schema.SourceOpenCode, "newer-builds-row", sources.CollectorVersion+2)
	cursor := cursorFor(t, st, schema.SourceCodex, home)

	backfillIfUpgraded(ctx, st, home, slog.New(slog.DiscardHandler))

	if v := recorded(t, st); v != strconv.Itoa(sources.CollectorVersion) {
		t.Fatalf("recorded version = %q, want %d -- left at the newer value, rolling "+
			"forward sees no gap and never re-reads what this build collects", v, sources.CollectorVersion)
	}
	if got := count(t, st); got != 1 {
		t.Errorf("a downgrade discarded rows: %d left, want 1", got)
	}
	if off, _, _ := st.Cursor(ctx, cursor); off == 0 {
		t.Error("a downgrade rewound a cursor")
	}
}

// openCodeHome is a home directory holding an opencode database, and respond
// writes (or rewrites) its one assistant response.
func openCodeHome(t *testing.T) (home string, respond func(input, updated int64)) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT)`)
	exec(`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`)
	return home, func(input, updated int64) {
		t.Helper()
		exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
			VALUES ('msg_1', 'ses_1', 1780000000000, ?, ?)
			ON CONFLICT(id) DO UPDATE SET time_updated = excluded.time_updated, data = excluded.data`,
			updated, fmt.Sprintf(`{"role":"assistant","modelID":"m","providerID":"p",`+
				`"tokens":{"input":%d,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},`+
				`"time":{"created":1780000000000}}`, input))
	}
}

// The reason a downgrade records its own version at all.
func TestARollbackIsReReadWhenRolledForward(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	home, respond := openCodeHome(t)
	t.Setenv("HOME", home)
	st, _ := newStore(t)

	// The newest version an older build could have been on and still have
	// opencode re-read, rather than purged, on the way back to this one.
	older := 0
	for v := sources.CollectorVersion - 1; v > 0 && older == 0; v-- {
		if slices.Contains(sources.SourcesNeedingBackfill(v), schema.SourceOpenCode) &&
			!slices.Contains(sources.SourcesNeedingPurge(v), schema.SourceOpenCode) {
			older = v
		}
	}
	if older == 0 {
		t.Fatal("no older collector version re-reads opencode on upgrade")
	}

	// This build collects the response.
	respond(1000, 1)
	if _, err := Run(ctx, st, home, log); err != nil {
		t.Fatal(err)
	}

	// Rolled back: the older build records its own version and stores the
	// grown response as its own reading, which is then uploaded.
	respond(1200, 2)
	id := schema.MakeID(schema.SourceOpenCode, "msg_1")
	e := schema.Event{V: schema.Version, ID: id, NativeID: "msg_1", Source: schema.SourceOpenCode,
		TS: time.UnixMilli(1780000000000).UTC(), Usage: schema.Usage{InputTokens: 1200}, Collector: older}
	if err := st.SetMeta(ctx, versionKey, strconv.Itoa(older)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitFile(ctx, "", 0, 0,
		[]store.Record{{ID: id, TS: e.TS, TotalTokens: 1200, Collector: older, Payload: e}},
		map[string]string{"opencode:message_watermark": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSent(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}

	// Rolled forward to this build.
	if _, err := Run(ctx, st, home, log); err != nil {
		t.Fatal(err)
	}

	_, payloads, err := st.Unsent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("%d rows queued for upload, want the re-read one -- the rollback's "+
			"reading was never replaced", len(payloads))
	}
	var got schema.Event
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.Collector != sources.CollectorVersion || got.Usage.TotalTokens() != 1200 {
		t.Fatalf("row holds collector %d's reading of %d tokens, want collector %d's of 1200",
			got.Collector, got.Usage.TotalTokens(), sources.CollectorVersion)
	}
}

// clineRow writes a Cline request under a given native id.
func clineRow(t *testing.T, st *store.Store, native string, at time.Time, input int64) string {
	t.Helper()
	e := schema.Event{
		V: schema.Version, ID: schema.MakeID(schema.SourceCline, native), NativeID: native,
		Source: schema.SourceCline, TS: at, MachineID: "m", SessionID: "task-1",
		Usage: schema.Usage{InputTokens: input},
	}
	if _, err := st.CommitFile(context.Background(), "", 0, 0, []store.Record{{
		ID: e.ID, TS: e.TS, TotalTokens: e.Usage.TotalTokens(), Collector: 9, Payload: e,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// A re-keyed row goes once the row replacing it is stored, and not before: a
// record deleted from disk keeps the only row it has. A source whose pass
// failed stays pending, since its re-read may not have written every
// replacement yet.
func TestReKeyedRowsAreDedupedOnlyAgainstTheirReplacement(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	at := time.Now().Truncate(time.Millisecond)

	for _, failed := range []bool{false, true} {
		st, _ := newStore(t)
		old := clineRow(t, st, "task-1#0", at, 100)
		replacement := clineRow(t, st, fmt.Sprintf("task-1#%d#0", at.UnixMilli()), at, 100)
		orphan := clineRow(t, st, "task-1#1", at.Add(time.Second), 200)
		if err := addDedupe(ctx, st, []schema.Source{schema.SourceCline}); err != nil {
			t.Fatal(err)
		}

		sum := &Summary{PerSource: map[schema.Source]SourceSummary{}}
		if failed {
			sum.PerSource[schema.SourceCline] = SourceSummary{Errors: 1}
		}
		dedupeSuperseded(ctx, st, sum, log)

		payloads, err := st.SourcePayloads(ctx, string(schema.SourceCline))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range payloads {
			var e schema.Event
			if err := json.Unmarshal(p, &e); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, e.ID)
		}
		if slices.Contains(ids, old) || !slices.Contains(ids, replacement) || !slices.Contains(ids, orphan) {
			t.Fatalf("failed=%v: stored %v; want the replacement and the orphan, not the re-keyed row", failed, ids)
		}
		if pending := pendingDedupe(ctx, st); failed != slices.Contains(pending, "cline") {
			t.Fatalf("failed=%v: pending = %v", failed, pending)
		}
	}
}

// Moved from a ChatGPT sign-in to an API key, Codex names no account, and the
// usage that follows must not be credited to the seat signed in before.
func TestUsageAfterASignOutIsNotCreditedToTheAccountBefore(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, path := newStore(t)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(".codex/auth.json", `{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-seat","id_token":""}}`)
	if _, err := Run(ctx, st, home, log); err != nil {
		t.Fatal(err)
	}
	// A pass later, as the daemon runs them: windows are keyed to the second.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`UPDATE account_window SET observed_at = observed_at - 300`); err != nil {
		t.Fatal(err)
	}

	write(".codex/auth.json", `{"OPENAI_API_KEY":"sk-test","tokens":null}`)
	at := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	write(".codex/sessions/2026/09/23/rollout-x.jsonl", fmt.Sprintf(
		`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{`+
			`"last_token_usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010},`+
			`"total_token_usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}}`+"\n", at))
	if _, err := Run(ctx, st, home, log); err != nil {
		t.Fatal(err)
	}

	_, payloads, err := st.Unsent(ctx, 10)
	if err != nil || len(payloads) != 1 {
		t.Fatalf("got %d events, err %v; want the one response", len(payloads), err)
	}
	var e schema.Event
	if err := json.Unmarshal(payloads[0], &e); err != nil {
		t.Fatal(err)
	}
	if e.AccountRef != "" || e.CostBasis != schema.CostUnknown {
		t.Fatalf("usage on an API key was credited to %q as %q; nobody was signed in",
			e.AccountRef, e.CostBasis)
	}
}

// An archive on collector 9 re-reads Continue under the machine-keyed ids and
// then drops each old row for its replacement; left beside it, the record
// counts twice.
func TestUpgradingToTenKeepsEachContinueRecordOnce(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, _ := newStore(t)

	p := filepath.Join(home, ".continue/dev_data/0.2.0/tokensGenerated.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-09-20T10:00:00Z","model":"claude-opus-5","promptTokens":100,"generatedTokens":10}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	// The archive as collector 9 left it: the record under its old id, and
	// the file read to its end.
	for k, v := range map[string]string{versionKey: "9", "machine_id": "m"} {
		if err := st.SetMeta(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	const old = "0.2.0/tokensGenerated.jsonl#0"
	e := schema.Event{V: schema.Version, ID: schema.MakeID(schema.SourceContinue, old), NativeID: old,
		Source: schema.SourceContinue, TS: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), MachineID: "m",
		Model: "claude-opus-5", Usage: schema.Usage{InputTokens: 100, OutputTokens: 10}, Collector: 9}
	if _, err := st.CommitFile(ctx, p, int64(len(line)), int64(len(line)), []store.Record{{
		ID: e.ID, TS: e.TS, TotalTokens: e.Usage.TotalTokens(), Collector: 9, Payload: e,
	}}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, st, home, log); err != nil {
		t.Fatal(err)
	}

	payloads, err := st.SourcePayloads(ctx, string(schema.SourceContinue))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, raw := range payloads {
		var got schema.Event
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, got.NativeID)
	}
	if want := []string{"m:" + old}; !slices.Equal(ids, want) {
		t.Fatalf("stored %q, want only %q", ids, want)
	}
}
