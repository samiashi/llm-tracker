package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/samiashi/llm-tracker/schema"
)

func atDay(id string, daysAgo int, tok int64) schema.Event {
	e := ev(id, tok, schema.CostBilled)
	e.TS = time.Now().AddDate(0, 0, -daysAgo)
	return e
}

// Old detail collapses into rollups; every total stays exact.
func TestPrunePreservesTotals(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	wide := Window{From: "2000-01-01", To: time.Now().AddDate(0, 0, 1).Format("2006-01-02")}

	ingest(t, d, atDay("old1", 60, 1000), atDay("old2", 45, 2000), atDay("new", 1, 500))

	before, err := d.Totals(ctx, wide)
	if err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	res, err := d.Prune(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsPruned != 2 {
		t.Fatalf("pruned %d events, want 2", res.EventsPruned)
	}

	after, err := d.Totals(ctx, wide)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalTokens != before.TotalTokens || after.Events != before.Events {
		t.Fatalf("totals changed: %d/%d tokens, %d/%d events",
			after.TotalTokens, before.TotalTokens, after.Events, before.Events)
	}
}

// A day must live in exactly one of raw events or rollups, or the view that
// unions them double-counts it.
func TestPruneIsIdempotent(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	wide := Window{From: "2000-01-01", To: time.Now().AddDate(0, 0, 1).Format("2006-01-02")}

	ingest(t, d, atDay("old", 60, 1000))
	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")

	if _, err := d.Prune(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	first, _ := d.Totals(ctx, wide)
	if _, err := d.Prune(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	second, _ := d.Totals(ctx, wide)

	if first.TotalTokens != second.TotalTokens {
		t.Fatalf("second prune changed totals: %d then %d", first.TotalTokens, second.TotalTokens)
	}
}

// The previous window is as long as the current one and ends the day before
// it: against a period of another size the percentage means nothing.
func TestCompareWindowAlignment(t *testing.T) {
	d := newDB(t)
	c, err := d.Compare(context.Background(), Window{From: "2026-09-15", To: "2026-09-21"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PreviousFrom != "2026-09-08" || c.PreviousTo != "2026-09-14" {
		t.Fatalf("prior window %s → %s, want 2026-09-08 → 2026-09-14",
			c.PreviousFrom, c.PreviousTo)
	}
}

// A floor from a new server's startup prune would make it refuse the backfill
// it is about to be sent.
func TestPruningAnEmptyDatabaseSetsNoFloor(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	cutoff := time.Now().AddDate(0, 0, -90).UTC().Format("2006-01-02")
	res, err := d.Prune(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if res.DaysRolled != 0 {
		t.Fatalf("rolled up %d days from an empty database", res.DaysRolled)
	}

	floor, err := d.RetentionFloor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if floor != "" {
		t.Fatalf("floor = %q on an empty database; it must stay unset", floor)
	}

	// The backfill lands.
	old := ev("backfill", 1_000, schema.CostBilled)
	old.ID, old.TS = "old-1", time.Now().AddDate(0, 0, -200)
	out := ingest(t, d, old)
	if out.EventsSkipped != 0 {
		t.Fatalf("a fresh server refused %d backfilled events", out.EventsSkipped)
	}
	if out.EventsStored != 1 {
		t.Fatalf("stored %d events, want 1", out.EventsStored)
	}
}

// A machine still checking in is kept however quiet it is: telling "not
// working" from "collector down" is the table's purpose.
func TestPruneForgetsMachinesThatStoppedReporting(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -200).Unix()
	now := time.Now().Unix()
	for _, m := range []struct {
		id       string
		lastSeen int64
	}{
		{"decommissioned", old},
		{"quiet-but-alive", now},
	} {
		if _, err := d.write.ExecContext(ctx, `
			INSERT INTO machine (id, hostname, agent_version, first_seen, last_seen)
			VALUES (?, ?, '', ?, ?)`, m.id, m.id, old, m.lastSeen); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -90).UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.MachineID] = true
	}
	if ids["decommissioned"] {
		t.Error("a machine that stopped reporting 200 days ago is still listed")
	}
	if !ids["quiet-but-alive"] {
		t.Error("a machine still checking in was dropped; quiet is not the same as gone")
	}
}

func mustHeat(t *testing.T, d *DB) []HeatCell {
	t.Helper()
	cells, _, err := d.Heatmap(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	return cells
}

func TestAPruningServerStillRefusesBelowItsFloor(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	old := ev("old", 1_000, schema.CostBilled)
	old.TS = time.Now().AddDate(0, 0, -200)
	ingest(t, d, old)
	if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -100).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	d.Pruning = true
	res, err := d.Ingest(ctx, &schema.Batch{
		V: schema.Version, MachineID: "m", Events: []schema.Event{old},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsSkipped != 1 {
		t.Fatalf("skipped %d, want the event refused while pruning is on", res.EventsSkipped)
	}
	if !res.RetentionEnforced || res.RetentionFloor == "" {
		t.Fatalf("reported enforced=%v floor=%q; an agent cannot retire what it "+
			"is not told about", res.RetentionEnforced, res.RetentionFloor)
	}

	// With pruning off nothing is refused (the rolled-up event is a no-op),
	// and the ack says so, so the agent offers what it held back.
	d.Pruning = false
	res, err = d.Ingest(ctx, &schema.Batch{
		V: schema.Version, MachineID: "m", Events: []schema.Event{old},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsSkipped != 0 || res.RetentionEnforced {
		t.Fatalf("skipped %d enforced=%v; a server keeping everything accepts everything",
			res.EventsSkipped, res.RetentionEnforced)
	}
}

func totalTokens(t *testing.T, d *DB) int64 {
	t.Helper()
	var n int64
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(total_tokens),0) FROM event_daily`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawCount(t *testing.T, d *DB) int64 {
	t.Helper()
	var n int64
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM event`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// oldDay returns n events of tok tokens each, all on one day 200 days ago.
func oldDay(prefix string, n int, tok int64) []schema.Event {
	day := time.Now().AddDate(0, 0, -200)
	out := make([]schema.Event, n)
	for i := range out {
		out[i] = ev(fmt.Sprintf("%s-%d", prefix, i), tok, schema.CostBilled)
		out[i].TS = day
	}
	return out
}

// pruneOld rolls up every day more than 100 days old.
func pruneOld(t *testing.T, d *DB) {
	t.Helper()
	if _, err := d.Prune(context.Background(),
		time.Now().AddDate(0, 0, -100).UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
}

// A rollup keeps sums, not ids: retiring it when one of its events comes back
// would drop every event that does not.
func TestAPartialRedeliveryNeverLowersAPrunedDay(t *testing.T) {
	d := newDB(t)
	evs := oldDay("p", 3, 1_000)
	ingest(t, d, evs...)
	pruneOld(t, d)
	before := totalTokens(t, d)

	// A collector bump re-reads one of them, longer: refused while pruning,
	// then re-offered once the server keeps everything.
	up := evs[0]
	up.Usage.InputTokens = 1_500
	d.Pruning = true
	if r := ingest(t, d, up); r.EventsSkipped != 1 {
		t.Fatalf("setup: skipped %d, want the reading refused while pruning", r.EventsSkipped)
	}
	d.Pruning = false
	ingest(t, d, up)

	if got := totalTokens(t, d); got < before {
		t.Fatalf("day total fell from %d to %d when one of its events came back", before, got)
	}
}

// An event the rollup never held -- from a laptop that was offline when the
// day was pruned -- belongs beside the rollup, not instead of it.
func TestALateEventAddsToAPrunedDay(t *testing.T) {
	d := newDB(t)
	ingest(t, d, oldDay("p", 3, 1_000)...)
	pruneOld(t, d)

	d.Pruning = false
	if res := ingest(t, d, oldDay("late", 1, 500)...); res.EventsStored != 1 {
		t.Fatalf("stored %d, want the late event stored", res.EventsStored)
	}
	if got := totalTokens(t, d); got != 3_500 {
		t.Fatalf("total = %d, want 3500: the late event adds to the day's rollup", got)
	}
}

// A rolled-up event is already counted, whatever machine id or login it
// arrives with now.
func TestResendingARolledUpEventIsANoOp(t *testing.T) {
	cases := []struct {
		name  string
		drift func(*schema.Event)
	}{
		{"as it was", func(*schema.Event) {}},
		{"from a regenerated machine id", func(e *schema.Event) { e.MachineID = "new-id" }},
		{"credited to another login", func(e *schema.Event) { e.AccountRef = "openai:b" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDB(t)
			claude := oldDay("claude", 3, 1_000)
			codex := oldDay("codex", 1, 3_000)[0]
			codex.Source, codex.AccountRef = schema.SourceCodex, "openai:personal"
			ingest(t, d, append(claude, codex)...)
			pruneOld(t, d)
			before := totalTokens(t, d)

			// Only Claude Code still holds the day, and re-delivers it.
			d.Pruning = false
			again := append([]schema.Event(nil), claude...)
			for i := range again {
				c.drift(&again[i])
			}
			res := ingest(t, d, again...)
			if res.EventsStored != 0 || res.EventsSkipped != 0 {
				t.Fatalf("stored %d, skipped %d: a rolled-up event is a successful "+
					"duplicate, neither stored again nor refused", res.EventsStored, res.EventsSkipped)
			}
			if got := totalTokens(t, d); got != before {
				t.Fatalf("total = %d after a re-send, want %d", got, before)
			}
		})
	}
}

// A missing id is a double count waiting for the next re-send.
func TestPruneRecordsEveryIdItRollsUp(t *testing.T) {
	d := newDB(t)
	evs := oldDay("p", 3, 1_000)
	ingest(t, d, append(evs, ev("recent", 10, schema.CostBilled))...)
	pruneOld(t, d)

	for _, e := range evs {
		var n int
		if err := d.read.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM pruned_event WHERE id = ?`, e.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("pruned event %s is not in the ledger", e.ID)
		}
	}
	var ledger int
	if err := d.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pruned_event`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != len(evs) {
		t.Fatalf("ledger holds %d ids, want %d: only rolled-up events belong in it", ledger, len(evs))
	}
}

// A day that gained late events since its prune is rolled up again with them,
// and their ids join the ledger, so a re-send after that is still a no-op.
func TestRePruningADayMergesItsLateEvents(t *testing.T) {
	d := newDB(t)
	ingest(t, d, oldDay("p", 3, 1_000)...)
	pruneOld(t, d)
	d.Pruning = false
	late := oldDay("late", 1, 500)
	ingest(t, d, late...)

	pruneOld(t, d)
	if got := totalTokens(t, d); got != 3_500 {
		t.Fatalf("total = %d after re-pruning, want 3500", got)
	}
	if n := rawCount(t, d); n != 0 {
		t.Fatalf("%d raw rows survived the second prune", n)
	}
	if res := ingest(t, d, late...); res.EventsStored != 0 {
		t.Fatalf("stored %d: the late event was rolled up and must be a no-op now", res.EventsStored)
	}
	if got := totalTokens(t, d); got != 3_500 {
		t.Fatalf("total = %d after re-sending the late event, want 3500", got)
	}
}

// openPrunedBefore00015 opens a database whose history was written at
// migration 00014 -- before pruned_event -- by the given statements.
func openPrunedBefore00015(t *testing.T, setup ...string) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(raw, "migrations", 14); err != nil {
		t.Fatal(err)
	}
	for _, q := range setup {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// legacyRollup is a rollup row as 00010 rebuilt the older ones: no machine.
func legacyRollup(day string, tok int) string {
	return fmt.Sprintf(`INSERT INTO daily_rollup (day, machine_id, account_ref,
		source, events, input_tokens, total_tokens, cost_basis)
		VALUES ('%s', '', 'anthropic:a', 'claude_code', 1, %d, %d, 'billed')`, day, tok, tok)
}

// The ack must report the refusal, or the agent re-offers those days forever.
func TestALegacyDayIsRefusedEvenWithRetentionOff(t *testing.T) {
	day := time.Now().AddDate(0, 0, -200).UTC().Format("2006-01-02")
	floor := time.Now().AddDate(0, 0, -199).UTC().Format("2006-01-02")
	d := openPrunedBefore00015(t,
		`INSERT INTO setting (key, value) VALUES ('retention_floor', '`+
			time.Now().AddDate(0, 0, -100).UTC().Format("2006-01-02")+`')`,
		legacyRollup(day, 1_000))
	d.Pruning = false

	// Another laptop on the same login catches up on that day.
	b := oldDay("laptop-b", 1, 500)[0]
	b.MachineID = "B"
	res := ingest(t, d, b)
	if res.EventsSkipped != 1 {
		t.Fatalf("skipped %d, want the legacy day refused", res.EventsSkipped)
	}
	if !res.RetentionEnforced || res.RetentionFloor != floor {
		t.Fatalf("ack floor=%q enforced=%v, want %q enforced: an agent re-offers "+
			"refused rows whenever the server says nothing is enforced",
			res.RetentionFloor, res.RetentionEnforced, floor)
	}
	if got := totalTokens(t, d); got != 1_000 {
		t.Fatalf("total = %d, want the legacy rollup's 1000 untouched", got)
	}

	// A day after the legacy floor is taken as usual.
	if res := ingest(t, d, ev("recent", 10, schema.CostBilled)); res.EventsStored != 1 {
		t.Fatalf("stored %d of a recent event", res.EventsStored)
	}
}

// The legacy floor covers every day that still has a rollup written without
// ids, and nothing else: a retention floor whose rollups are gone protects
// nothing, and would refuse every re-read of those days for good.
func TestTheLegacyFloorCoversEveryRollupWithoutIds(t *testing.T) {
	day := time.Now().AddDate(0, 0, -200).UTC()
	next := day.AddDate(0, 0, 1).Format("2006-01-02")
	retention := `INSERT INTO setting (key, value) VALUES ('retention_floor', '` +
		time.Now().AddDate(0, 0, -100).UTC().Format("2006-01-02") + `')`
	cases := []struct {
		name  string
		setup []string
		want  string
	}{
		{"a retention floor past the last rollup", []string{
			retention, legacyRollup(day.Format("2006-01-02"), 1_000)}, next},
		{"a rollup with no floor recorded", []string{
			legacyRollup(day.Format("2006-01-02"), 1_000)}, next},
		{"a retention floor with every rollup gone", []string{retention}, ""},
		{"nothing rolled up", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := openPrunedBefore00015(t, c.setup...)
			d.Pruning = false
			res := ingest(t, d, oldDay("probe", 1, 10)...)
			if res.RetentionFloor != c.want {
				t.Fatalf("floor = %q, want %q", res.RetentionFloor, c.want)
			}
			if wantSkipped := boolInt(c.want != ""); res.EventsSkipped != wantSkipped {
				t.Fatalf("skipped %d, want %d", res.EventsSkipped, wantSkipped)
			}
		})
	}
}

// A late event beside a rolled-up day is a sliver of it; starting there, the
// heatmap would draw the day at a fraction of its total.
func TestTheHeatmapStopsAtRolledUpDays(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, oldDay("p", 3, 1_000)...)
	pruneOld(t, d)
	ingest(t, d, ev("recent", 700, schema.CostBilled))

	// A late event beside the rollup, as ingest stores one with pruning off.
	day := time.Now().AddDate(0, 0, -200)
	if _, err := d.write.ExecContext(ctx, `
		INSERT INTO event (id, source, ts, day, machine_id, account_ref, model,
		                   input_tokens, total_tokens, cost_basis, received_at)
		VALUES ('late', 'claude_code', ?, ?, 'm', 'anthropic:a', 'claude-opus-5',
		        500, 500, 'billed', 0)`,
		day.Unix(), day.UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	first, err := d.EarliestRawDay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var heat int64
	for _, c := range mustHeat(t, d) {
		heat += c.Tokens
	}
	tot, err := d.Totals(ctx, Window{From: first})
	if err != nil {
		t.Fatal(err)
	}
	if heat != tot.TotalTokens {
		t.Fatalf("heatmap = %d tokens from %s, totals over the same days = %d",
			heat, first, tot.TotalTokens)
	}
}

// A floor enforced but not reported re-sends the refused backlog on every
// push; one reported but not enforced strands rows the server would take.
func TestTheAckReportsExactlyTheFloorIngestEnforces(t *testing.T) {
	daysAgo := func(n int) string { return time.Now().AddDate(0, 0, -n).UTC().Format("2006-01-02") }
	cases := []struct {
		name                    string
		pruning, pruned, legacy bool
		want                    string
	}{
		{"keeps everything", false, false, false, ""},
		{"prunes, nothing pruned yet", true, false, false, ""},
		{"prunes", true, true, false, daysAgo(100)},
		{"stopped pruning", false, true, false, ""},
		{"stopped pruning, with legacy rollups", false, true, true, daysAgo(300)},
		{"prunes, with legacy rollups", true, true, true, daysAgo(100)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDB(t)
			if c.legacy {
				if _, err := d.write.Exec(`INSERT INTO setting (key, value)
					VALUES ('legacy_rollup_floor', ?)`, daysAgo(300)); err != nil {
					t.Fatal(err)
				}
			}
			if c.pruned {
				ingest(t, d, oldDay("p", 1, 1_000)...)
				pruneOld(t, d)
			}
			d.Pruning = c.pruning

			var probes []schema.Event
			for _, age := range []int{400, 250, 150, 50, 0} {
				e := ev(fmt.Sprintf("probe-%d", age), 10, schema.CostBilled)
				e.TS = time.Now().AddDate(0, 0, -age)
				probes = append(probes, e)
			}
			res := ingest(t, d, probes...)

			if res.RetentionFloor != c.want {
				t.Fatalf("floor = %q, want %q", res.RetentionFloor, c.want)
			}
			if res.RetentionEnforced != (res.RetentionFloor != "") {
				t.Fatalf("enforced = %v with floor %q", res.RetentionEnforced, res.RetentionFloor)
			}
			below := 0
			for _, e := range probes {
				if e.TS.UTC().Format("2006-01-02") < res.RetentionFloor {
					below++
				}
			}
			if res.EventsSkipped != below {
				t.Fatalf("skipped %d, but %d events fall below the reported floor %q",
					res.EventsSkipped, below, res.RetentionFloor)
			}
		})
	}
}

// Codex rows still on the ordinal key return under content keys when their
// machine upgrades, and the ledger cannot match those to the rollup that
// already counts them: a prune closes their days instead. A machine already
// on the new key leaves the days open for genuinely late events.
func TestPruningOldCodexKeysClosesTheirDays(t *testing.T) {
	ctx := context.Background()
	day := time.Now().AddDate(0, 0, -200)
	codex := func(native string) schema.Event {
		e := ev(native, 1_000, schema.CostRateCard)
		e.Source, e.NativeID, e.TS = schema.SourceCodex, native, day
		return e
	}

	for _, upgraded := range []bool{false, true} {
		d := newDB(t)
		first := codex("rollout-a.jsonl#5")
		if upgraded {
			first = codex("tc:first")
		}
		ingest(t, d, first)
		if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -100).Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
		d.Pruning = false

		res := ingest(t, d, codex("tc:rekeyed"))
		if want := !upgraded; (res.EventsSkipped == 1) != want {
			t.Fatalf("upgraded=%v: skipped %d; the re-keyed event must be refused only while "+
				"its old-key twin sits in the rollup", upgraded, res.EventsSkipped)
		}
		if want := int64(1_000); !upgraded && totalTokens(t, d) != want {
			t.Fatalf("total = %d, want %d: the day was counted twice", totalTokens(t, d), want)
		}
	}
}

// Only rolled-up days keep the prices they were rolled up at: a retention
// floor whose rollups are gone must not warn that a reprice missed anything.
func TestRollupsBeforeNamesOnlyRolledUpDays(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	if day, err := d.RollupsBefore(ctx); err != nil || day != "" {
		t.Fatalf("with nothing rolled up: %q (%v), want none", day, err)
	}
	ingest(t, d, oldDay("p", 1, 1_000)...)
	pruneOld(t, d)
	want := time.Now().AddDate(0, 0, -199).UTC().Format(time.DateOnly)
	if day, err := d.RollupsBefore(ctx); err != nil || day != want {
		t.Fatalf("after a prune: %q (%v), want %s", day, err, want)
	}
}
