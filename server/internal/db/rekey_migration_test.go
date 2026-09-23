package db

import (
	"cmp"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/samiashi/llm-tracker/schema"
)

var rekeyAt = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

func keyed(src schema.Source, machine, nid, session string, ts time.Time, in int64) schema.Event {
	return schema.Event{
		V: schema.Version, ID: schema.MakeID(src, nid), NativeID: nid, Source: src,
		TS: ts, MachineID: machine, AccountRef: "openai:a", Model: "m", SessionID: session,
		CostBasis: schema.CostRateCard, Usage: schema.Usage{InputTokens: in, OutputTokens: 1},
	}
}

// ingestFrom uploads events as machine does: every row is stored under the
// machine its batch names.
func ingestFrom(t *testing.T, d *DB, machine string, events ...schema.Event) {
	t.Helper()
	if _, err := d.Ingest(context.Background(), testLogin, &schema.Batch{
		V: schema.Version, MachineID: machine, Events: events,
	}); err != nil {
		t.Fatal(err)
	}
}

// storeRaw writes events as a server on an older schema stored them, which
// ingest, following the current one, cannot.
func storeRaw(t *testing.T, d *DB, events ...schema.Event) {
	t.Helper()
	for _, e := range events {
		if _, err := d.write.ExecContext(context.Background(), `
			INSERT INTO event (id, native_id, source, ts, day, machine_id, account_ref, model,
			                   session_id, input_tokens, output_tokens, total_tokens,
			                   cost_basis, collector, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			e.ID, e.NativeID, string(e.Source), e.TS.Unix(), e.TS.UTC().Format(time.DateOnly),
			e.MachineID, e.AccountRef, e.Model, e.SessionID, e.Usage.InputTokens,
			e.Usage.OutputTokens, e.Usage.TotalTokens(), string(e.CostBasis), e.Collector); err != nil {
			t.Fatal(err)
		}
	}
}

func nativeIDs(t *testing.T, d *DB, src schema.Source, machine string) []string {
	t.Helper()
	rows, err := d.write.QueryContext(context.Background(),
		`SELECT native_id FROM event WHERE source = ? AND machine_id = ? ORDER BY native_id`,
		string(src), machine)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// withoutRekeyMigration rolls back to just before 00016, runs fill against
// that schema, and migrates up again, so 00016's one-time pass sees what fill
// stored.
func withoutRekeyMigration(t *testing.T, d *DB, fill func()) {
	t.Helper()
	withoutMigrationsAfter(t, d, 15, fill)
}

// withoutMigrationsAfter rolls back to version, runs fill against that
// schema, and migrates up again. DownTo, not Down: Down undoes only the
// latest migration, which a test's own stops being once another is added.
func withoutMigrationsAfter(t *testing.T, d *DB, version int64, fill func()) {
	t.Helper()
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(d.write, "migrations", version); err != nil {
		t.Fatal(err)
	}
	fill()
	if err := goose.Up(d.write, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// Event ids carry no machine, so each machine's rollouts are named apart, as
// their UUIDs make them in practice.
func absKey(machine string) string {
	return "/Users/dev/.codex/sessions/2026/09/20/" + baseKey(machine)
}

func baseKey(machine string) string {
	return "rollout-2026-09-20T10-00-00-019e-" + machine + ".jsonl#5"
}

// Collector 9 keys Codex token_count usage on its content. The ordinal-keyed
// rows it replaces cannot be paired with the new ones, so a machine's go when
// it first reports the new key -- not before, or its Codex history would
// vanish until it upgrades. Response-keyed rows are a different record.
func TestCodexOrdinalKeysRetireWhenTheirMachineReportsTheNewKey(t *testing.T) {
	d := newDB(t)
	for _, m := range []string{"m1", "m2"} {
		ingestFrom(t, d, m,
			keyed(schema.SourceCodex, m, absKey(m), "", rekeyAt, 100),
			keyed(schema.SourceCodex, m, baseKey(m), "", rekeyAt, 100),
			keyed(schema.SourceCodex, m, "resp_0a1b"+m, "s", rekeyAt, 50),
		)
	}

	ingestFrom(t, d, "m1", keyed(schema.SourceCodex, "m1", "tc:5f0c", "s", rekeyAt, 100))
	if got, want := nativeIDs(t, d, schema.SourceCodex, "m1"), []string{"resp_0a1bm1", "tc:5f0c"}; !slices.Equal(got, want) {
		t.Fatalf("m1 holds %v, want %v", got, want)
	}
	if got := nativeIDs(t, d, schema.SourceCodex, "m2"); len(got) != 3 {
		t.Fatalf("m2 holds %v; a machine still on the old key must keep its rows", got)
	}

	// A straggler under the old key from a machine that has moved on.
	ingestFrom(t, d, "m1", keyed(schema.SourceCodex, "m1", "rollout-x.jsonl#9", "", rekeyAt, 70))
	if got := nativeIDs(t, d, schema.SourceCodex, "m1"); slices.Contains(got, "rollout-x.jsonl#9") {
		t.Fatalf("m1 holds %v; an old-key row after the new key would be counted twice", got)
	}
}

// Rows stored before the server upgraded are handled by the migration itself:
// machines already on the new key lose their ordinal-keyed rows, and on the
// rest only a pre-collector-5 row whose basename twin is present goes.
func TestCodexRowsStoredBeforeTheMigrationAreRetired(t *testing.T) {
	d := newDB(t)
	withoutRekeyMigration(t, d, func() {
		storeRaw(t, d,
			keyed(schema.SourceCodex, "m1", absKey("m1"), "", rekeyAt, 100),
			keyed(schema.SourceCodex, "m1", "tc:5f0c", "s", rekeyAt, 100),
			keyed(schema.SourceCodex, "m2", absKey("m2"), "", rekeyAt, 100),
			keyed(schema.SourceCodex, "m2", baseKey("m2"), "", rekeyAt, 100),
			keyed(schema.SourceCodex, "m2", "/Users/dev/.codex/sessions/rollout-gone.jsonl#3", "", rekeyAt, 40),
		)
	})

	if got, want := nativeIDs(t, d, schema.SourceCodex, "m1"), []string{"tc:5f0c"}; !slices.Equal(got, want) {
		t.Errorf("m1 holds %v, want %v", got, want)
	}
	want := []string{"/Users/dev/.codex/sessions/rollout-gone.jsonl#3", baseKey("m2")}
	if got := nativeIDs(t, d, schema.SourceCodex, "m2"); !slices.Equal(got, want) {
		t.Errorf("m2 holds %v, want %v", got, want)
	}
}

// Cline rewrites its task files, so an old '<task>#<index>' row goes only where
// the row now keyed on the same request is present. Its request may be gone
// from disk, and then the old row is the only record left.
func TestClineOldKeysRetireOnlyWhereTheirReplacementIsPresent(t *testing.T) {
	later := rekeyAt.Add(time.Minute)
	old := []schema.Event{
		keyed(schema.SourceCline, "m", "task#4", "task", rekeyAt, 10),
		keyed(schema.SourceCline, "m", "task#7", "task", rekeyAt, 10), // the same request, index shifted
		keyed(schema.SourceCline, "m", "task#9", "task", later, 20),   // its request is gone
	}
	current := keyed(schema.SourceCline, "m", "task#1790000001000#0", "task", rekeyAt, 10)
	want := []string{"task#1790000001000#0", "task#9"}

	t.Run("replacement arrives later", func(t *testing.T) {
		d := newDB(t)
		ingest(t, d, old...)
		ingest(t, d, current)
		if got := nativeIDs(t, d, schema.SourceCline, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
	t.Run("old row arrives later", func(t *testing.T) {
		d := newDB(t)
		ingest(t, d, current)
		ingest(t, d, old...)
		if got := nativeIDs(t, d, schema.SourceCline, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
	t.Run("both stored before the migration", func(t *testing.T) {
		d := newDB(t)
		withoutRekeyMigration(t, d, func() { storeRaw(t, d, append(old, current)...) })
		if got := nativeIDs(t, d, schema.SourceCline, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
}

// continueRow is a Continue record as the collector version given keys it:
// model "m", 1 output token.
func continueRow(nid string, collector int, ts time.Time, in int64) schema.Event {
	e := keyed(schema.SourceContinue, "m", nid, "", ts, in)
	e.Collector = collector
	return e
}

const (
	continuePath = "/Users/dev/.continue/dev_data/0.2.0/tokensGenerated.jsonl#20260920T100000.000#m"
	continueBase = "tokensGenerated.jsonl#0"
	continueRel  = "0.2.0/tokensGenerated.jsonl#0"
)

// Continue's collector-10 key names the machine. Each older key gives way to
// the row now keyed on the same record, and only where that row is present:
// a path-keyed record to any later row at its time, a basename to a row
// ending in it, a collector-9 row to its own machine's "m:" row. Anything
// unmatched may be the only record left, and stays.
func TestContinueOldKeysRetireOnlyWhereTheirReplacementIsPresent(t *testing.T) {
	later := rekeyAt.Add(time.Minute)
	old := []schema.Event{
		continueRow(continuePath, 4, rekeyAt, 10),
		continueRow(continueBase, 8, rekeyAt, 10),
		continueRow(continueRel, 9, rekeyAt, 10),
		continueRow("tokensGenerated.jsonl#128", 9, later, 30), // a root file's record
		continueRow("tokensGenerated.jsonl#64", 8, later, 40),  // no replacement
	}
	current := []schema.Event{
		continueRow("m:"+continueRel, 10, rekeyAt, 10),
		continueRow("m:tokensGenerated.jsonl#128", 10, later, 30),
	}
	want := []string{"m:0.2.0/tokensGenerated.jsonl#0", "m:tokensGenerated.jsonl#128", "tokensGenerated.jsonl#64"}

	t.Run("replacement arrives later", func(t *testing.T) {
		d := newDB(t)
		ingest(t, d, old...)
		ingest(t, d, current...)
		if got := nativeIDs(t, d, schema.SourceContinue, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
	t.Run("old row arrives later", func(t *testing.T) {
		d := newDB(t)
		ingest(t, d, current...)
		ingest(t, d, old...)
		if got := nativeIDs(t, d, schema.SourceContinue, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
	t.Run("both stored before the migration", func(t *testing.T) {
		d := newDB(t)
		withoutMigrationsAfter(t, d, 22, func() { ingest(t, d, append(old, current...)...) })
		if got := nativeIDs(t, d, schema.SourceContinue, "m"); !slices.Equal(got, want) {
			t.Fatalf("holds %v, want %v", got, want)
		}
	})
}

// The server's copy of sources.Superseded: an old row that matches a current
// one in all but one part of the key is a different record, and may be the
// only one of it left. Checked in both arrival orders, which run different
// triggers.
func TestRekeyTriggersSpareARowThatDiffersInAnyPartOfItsKey(t *testing.T) {
	later := rekeyAt.Add(time.Second)
	clineNow := keyed(schema.SourceCline, "m", "task#1790000001000#0", "task", rekeyAt, 10)
	contNow := continueRow("m:"+continueRel, 10, rekeyAt, 10)
	with := func(e schema.Event, change func(*schema.Event)) schema.Event {
		change(&e)
		return e
	}
	for _, tc := range []struct {
		name     string
		old, now schema.Event
		machine  string // the old row's, when not the current one's
	}{
		{name: "cline: another request at the same time",
			old: keyed(schema.SourceCline, "m", "task#2", "task", rekeyAt, 11), now: clineNow},
		{name: "cline: the same usage at another time",
			old: keyed(schema.SourceCline, "m", "task#5", "task", later, 10), now: clineNow},
		{name: "cline: another task",
			old: keyed(schema.SourceCline, "m", "gone#1", "gone", rekeyAt, 10), now: clineNow},

		{name: "continue path key: another time",
			old: continueRow(continuePath, 4, later, 10), now: contNow},
		{name: "continue path key: another model",
			old: with(continueRow(continuePath, 4, rekeyAt, 10), func(e *schema.Event) { e.Model = "m2" }), now: contNow},
		{name: "continue path key: other input",
			old: continueRow(continuePath, 4, rekeyAt, 11), now: contNow},
		{name: "continue path key: other output",
			old: with(continueRow(continuePath, 4, rekeyAt, 10), func(e *schema.Event) { e.Usage.OutputTokens = 2 }), now: contNow},
		{name: "continue path key: another machine",
			old: continueRow(continuePath, 4, rekeyAt, 10), now: contNow, machine: "m2"},

		{name: "continue basename: another offset",
			old: continueRow("tokensGenerated.jsonl#64", 8, rekeyAt, 10), now: contNow},
		{name: "continue basename: a file whose name it ends",
			old: continueRow("Generated.jsonl#0", 8, rekeyAt, 10), now: contNow},
		{name: "continue basename: another model",
			old: with(continueRow(continueBase, 8, rekeyAt, 10), func(e *schema.Event) { e.Model = "m2" }), now: contNow},
		{name: "continue basename: other input",
			old: continueRow(continueBase, 8, rekeyAt, 11), now: contNow},
		{name: "continue basename: other output",
			old: with(continueRow(continueBase, 8, rekeyAt, 10), func(e *schema.Event) { e.Usage.OutputTokens = 2 }), now: contNow},
		{name: "continue basename: another machine",
			old: continueRow(continueBase, 8, rekeyAt, 10), now: contNow, machine: "m2"},
		{name: "continue basename shape: a root file's collector-9 key",
			old: continueRow(continueBase, 9, rekeyAt, 10), now: contNow},

		{name: "continue collector 9: another offset",
			old: continueRow("0.2.0/tokensGenerated.jsonl#64", 9, rekeyAt, 10), now: contNow},
		{name: "continue collector 9: another directory",
			old: continueRow("0.1.0/tokensGenerated.jsonl#0", 9, rekeyAt, 10), now: contNow},
		{name: "continue collector 9: another machine",
			old: continueRow(continueRel, 9, rekeyAt, 10), now: contNow, machine: "m2"},
		{name: "continue collector 9: a replacement not from collector 10",
			old: continueRow(continueRel, 9, rekeyAt, 10), now: continueRow("m:"+continueRel, 9, rekeyAt, 10)},
	} {
		oldMachine := cmp.Or(tc.machine, "m")
		for _, order := range []string{"old first", "old last"} {
			t.Run(tc.name+", "+order, func(t *testing.T) {
				d := newDB(t)
				if order == "old first" {
					ingestFrom(t, d, oldMachine, tc.old)
					ingestFrom(t, d, "m", tc.now)
				} else {
					ingestFrom(t, d, "m", tc.now)
					ingestFrom(t, d, oldMachine, tc.old)
				}
				if got := nativeIDs(t, d, tc.old.Source, oldMachine); !slices.Contains(got, tc.old.NativeID) {
					t.Fatalf("holds %v: %s was retired though nothing replaces it", got, tc.old.NativeID)
				}
			})
		}
	}
}

// A day before the legacy floor is refused at ingest, so its old-key rows must
// stay: the re-keyed replacement will never be admitted to take their place.
func TestCodexOrdinalKeysBelowTheLegacyFloorAreKept(t *testing.T) {
	before, after := rekeyAt.AddDate(0, 0, -1), rekeyAt.AddDate(0, 0, 1)
	d := newDB(t)
	withoutRekeyMigration(t, d, func() {
		storeRaw(t, d,
			keyed(schema.SourceCodex, "m1", "rollout-m1.jsonl#1", "", before, 100),
			keyed(schema.SourceCodex, "m1", "rollout-m1.jsonl#2", "", after, 100),
			keyed(schema.SourceCodex, "m1", "tc:5f0c", "s", after, 100),
			keyed(schema.SourceCodex, "m2", "rollout-m2.jsonl#1", "", before, 100),
			keyed(schema.SourceCodex, "m2", "rollout-m2.jsonl#2", "", after, 100),
		)
		// As 00015 records it when the last rollup day is 2026-09-19.
		if _, err := d.write.ExecContext(context.Background(),
			`INSERT INTO setting (key, value) VALUES ('legacy_rollup_floor', '2026-09-20')`); err != nil {
			t.Fatal(err)
		}
	})

	var floor string
	if err := d.write.QueryRowContext(context.Background(),
		`SELECT value FROM setting WHERE key = 'legacy_rollup_floor'`).Scan(&floor); err != nil || floor != "2026-09-20" {
		t.Fatalf("legacy_rollup_floor = %q (%v), want 2026-09-20 untouched", floor, err)
	}
	if got, want := nativeIDs(t, d, schema.SourceCodex, "m1"), []string{"rollout-m1.jsonl#1", "tc:5f0c"}; !slices.Equal(got, want) {
		t.Errorf("m1 holds %v, want %v", got, want)
	}

	ingestFrom(t, d, "m2", keyed(schema.SourceCodex, "m2", "tc:9d2e", "s", after, 100))
	if got, want := nativeIDs(t, d, schema.SourceCodex, "m2"), []string{"rollout-m2.jsonl#1", "tc:9d2e"}; !slices.Equal(got, want) {
		t.Errorf("m2 holds %v, want %v", got, want)
	}
}
