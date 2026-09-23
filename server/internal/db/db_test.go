package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/samiashi/llm-tracker/schema"
)

func newDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func ev(id string, tok int64, basis schema.CostBasis) schema.Event {
	return schema.Event{
		V: schema.Version, ID: id, Source: schema.SourceClaudeCode,
		TS: time.Now(), MachineID: "m", AccountRef: "anthropic:a",
		Model: "claude-opus-5", CostBasis: basis,
		Usage: schema.Usage{InputTokens: tok},
	}
}

func ingest(t *testing.T, d *DB, events ...schema.Event) *IngestResult {
	t.Helper()
	res, err := d.Ingest(context.Background(), &schema.Batch{
		V: schema.Version, MachineID: "m", Events: events,
		Accounts: []schema.Account{{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// This is what lets the agent retry a batch freely after a network failure.
func TestIngestIsIdempotent(t *testing.T) {
	d := newDB(t)
	e := ev("a", 100, schema.CostBilled)

	ingest(t, d, e)
	second := ingest(t, d, e)
	if second.EventsStored != 0 {
		t.Fatalf("re-sending stored %d rows, want 0", second.EventsStored)
	}

	total, err := d.Totals(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if total.Events != 1 || total.InputTokens != 100 {
		t.Fatalf("got %d events / %d tokens, want 1 / 100", total.Events, total.InputTokens)
	}
}

// Invariant 3: billed and rate-card spend are never summed.
func TestBilledAndRateCardStaySeparate(t *testing.T) {
	d := newDB(t)
	ingest(t, d,
		ev("billed", 1_000_000, schema.CostBilled),
		ev("seat", 1_000_000, schema.CostRateCard),
	)

	total, err := d.Totals(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if total.BilledUSD <= 0 || total.RateCardUSD <= 0 {
		t.Fatalf("both buckets should be populated: billed=%v ratecard=%v",
			total.BilledUSD, total.RateCardUSD)
	}
	// Identical usage on each basis, so each bucket holds the same figure.
	if total.BilledUSD != total.RateCardUSD {
		t.Fatalf("identical usage on each basis gave billed=%v ratecard=%v",
			total.BilledUSD, total.RateCardUSD)
	}
	// And neither may carry the other's value folded in.
	sum := total.BilledUSD + total.RateCardUSD
	if total.BilledUSD == sum || total.RateCardUSD == sum {
		t.Fatalf("a bucket holds the sum of both: billed=%v ratecard=%v sum=%v",
			total.BilledUSD, total.RateCardUSD, sum)
	}
}

// Invariant 8: costed at zero, the models most worth noticing would look free.
func TestUnknownModelSurfacesAsUnpriced(t *testing.T) {
	d := newDB(t)
	e := ev("x", 5_000, schema.CostRateCard)
	e.Model = "not-a-real-model"
	res := ingest(t, d, e)

	if res.Unpriced != 1 {
		t.Fatalf("got %d unpriced, want 1", res.Unpriced)
	}
	total, _ := d.Totals(context.Background(), Window{From: "2000-01-01"})
	if total.UnpricedTokens != 5_000 {
		t.Fatalf("unpriced tokens = %d, want 5000", total.UnpricedTokens)
	}
}

// The harness's own figure wins even when the token counts are unchanged.
func TestNativeCostReplacesTableEstimate(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	ingest(t, d, ev("n", 1_000_000, schema.CostBilled))

	withNative := ev("n", 1_000_000, schema.CostBilled)
	native := 0.42
	withNative.NativeCostUSD = &native
	ingest(t, d, withNative)

	total, _ := d.Totals(ctx, Window{From: "2000-01-01"})
	if total.BilledUSD != native {
		t.Fatalf("billed = %v, want the harness's own figure %v", total.BilledUSD, native)
	}
}

// Without the machine's account, the event's spend belongs to nobody.
func TestUnattributedEventGetsMachineAccount(t *testing.T) {
	d := newDB(t)
	e := ev("orphan", 10, schema.CostBilled)
	e.AccountRef = ""
	ingest(t, d, e)

	groups, err := d.ByPerson(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Key != "dev@example.com" {
		t.Fatalf("got %+v, want the event credited to the machine's owner", groups)
	}
}

func TestAccountsMergeIntoOnePerson(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	a := ev("a", 10, schema.CostBilled)
	b := ev("b", 20, schema.CostBilled)
	b.AccountRef = "openai:b"

	_, err := d.Ingest(ctx, &schema.Batch{V: schema.Version, MachineID: "m",
		Events: []schema.Event{a, b}, Accounts: []schema.Account{
			{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"},
			{Ref: "openai:b", Provider: "openai", Email: "dev@example.com"},
		}})
	if err != nil {
		t.Fatal(err)
	}

	people, _ := d.ByPerson(ctx, Window{From: "2000-01-01"})
	if len(people) != 1 {
		t.Fatalf("got %d people, want 1 -- two accounts, one colleague", len(people))
	}
	if people[0].Totals.InputTokens != 30 {
		t.Fatalf("merged tokens = %d, want 30", people[0].Totals.InputTokens)
	}

}

// A harness that gains an adapter must stop being reported as a gap.
func TestCompleteUnknownReportReplaces(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	send := func(complete bool, hints ...string) {
		t.Helper()
		var rows []schema.UnknownSource
		for _, h := range hints {
			rows = append(rows, schema.UnknownSource{
				V: schema.Version, MachineID: "m", Path: "/p/" + h, Hint: h, SizeBytes: 1,
			})
		}
		if _, err := d.Ingest(ctx, &schema.Batch{
			V: schema.Version, MachineID: "m",
			UnknownSource: rows, UnknownComplete: complete,
		}); err != nil {
			t.Fatal(err)
		}
	}

	send(true, "gemini", "antigravity")
	if rows, _ := d.UnknownSources(ctx); len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	// gemini now has an adapter, so the agent stops reporting it.
	send(true, "antigravity")
	rows, err := d.UnknownSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hint != "antigravity" {
		t.Fatalf("got %+v, want only antigravity -- the stale row must be dropped", rows)
	}

	// An empty complete report clears the machine entirely.
	send(true)
	if rows, _ := d.UnknownSources(ctx); len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// A dimension on a column event_daily does not carry is a 500, even while no
// card uses it.
func TestEveryBreakdownDimensionRuns(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, ev("a", 100, schema.CostBilled))
	w := Window{From: "2000-01-01", To: "2099-01-01"}

	for name, fn := range map[string]func(context.Context, Window) ([]Group, error){
		"person":  d.ByPerson,
		"model":   d.ByModel,
		"source":  d.BySource,
		"surface": d.BySurface,
		"origin":  d.ByOrigin,
	} {
		if _, err := fn(ctx, w); err != nil {
			t.Errorf("breakdown %q failed: %v", name, err)
		}
	}
}

// Every matrix dimension must be queryable against the view too.
func TestEveryMatrixDimensionRuns(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, ev("a", 100, schema.CostBilled))
	w := Window{From: "2000-01-01", To: "2099-01-01"}

	for dim := range matrixDims {
		if _, err := d.Matrix(ctx, w, dim, "effort", 6); err != nil {
			t.Errorf("matrix rows=%q failed: %v", dim, err)
		}
		if _, err := d.Matrix(ctx, w, "model", dim, 6); err != nil {
			t.Errorf("matrix cols=%q failed: %v", dim, err)
		}
	}
	if _, err := d.Matrix(ctx, w, "nonsense", "effort", 6); err == nil {
		t.Error("an unknown dimension must be rejected, not interpolated into SQL")
	}
}

// Widening retention -- prune at 30 days, then at 90 -- must not re-open days
// already inside a rollup.
func TestRetentionFloorNeverMovesBackwards(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	old := ev("old", 1_000, schema.CostBilled)
	old.TS = time.Now().AddDate(0, 0, -60)
	old.ID = "old-event"
	ingest(t, d, old)

	before30 := time.Now().AddDate(0, 0, -30).UTC().Format("2006-01-02")
	if _, err := d.Prune(ctx, before30); err != nil {
		t.Fatal(err)
	}
	after, err := d.RetentionFloor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before30 {
		t.Fatalf("floor = %q, want %q", after, before30)
	}
	first, _ := d.Totals(ctx, Window{From: "2000-01-01"})

	// Nothing is older than 90 days, so this prune rolls up nothing.
	before90 := time.Now().AddDate(0, 0, -90).UTC().Format("2006-01-02")
	if _, err := d.Prune(ctx, before90); err != nil {
		t.Fatal(err)
	}
	floor, err := d.RetentionFloor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if floor != before30 {
		t.Fatalf("floor moved backwards to %q; it must stay at %q", floor, before30)
	}

	// A server still pruning refuses the resend; one that keeps everything
	// treats it as a duplicate (TestResendingARolledUpEventIsANoOp).
	d.Pruning = true
	res := ingest(t, d, old)
	if res.EventsSkipped != 1 {
		t.Fatalf("a rolled-up event was accepted again: skipped=%d", res.EventsSkipped)
	}
	second, _ := d.Totals(ctx, Window{From: "2000-01-01"})
	if second.TotalTokens != first.TotalTokens {
		t.Fatalf("totals changed after a resend: %d -> %d",
			first.TotalTokens, second.TotalTokens)
	}
}

// Such a day is normal with Anthropic. If daily_rollup's key and Prune's
// GROUP BY disagree on inference_geo, every prune aborts on it.
func TestPruneHandlesADayThatMixesInferenceGeo(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	at := time.Now().AddDate(0, 0, -60)
	us := ev("us", 1_000, schema.CostBilled)
	us.ID, us.TS, us.InferenceGeo = "geo-us", at, "us"
	any := ev("any", 2_000, schema.CostBilled)
	any.ID, any.TS, any.InferenceGeo = "geo-any", at, ""
	ingest(t, d, us, any)

	before, _ := d.Totals(ctx, Window{From: "2000-01-01"})

	if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -30).UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("prune failed on a day with two inference_geo values: %v", err)
	}

	after, err := d.Totals(ctx, Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalTokens != before.TotalTokens {
		t.Fatalf("rollup changed the totals: %d -> %d",
			before.TotalTokens, after.TotalTokens)
	}
}

// Every query filters on day: an event on 0001-01-01 is invisible everywhere,
// while SourceHealth reads its substituted ts as fresh.
func TestAZeroTimestampLandsOnTodayNotYearOne(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	e := ev("no-ts", 1_234, schema.CostBilled)
	e.TS = time.Time{}
	ingest(t, d, e)

	today := time.Now().UTC().Format("2006-01-02")
	total, err := d.Totals(ctx, Window{From: today, To: today})
	if err != nil {
		t.Fatal(err)
	}
	if total.TotalTokens != 1_234 {
		t.Fatalf("a zero-timestamp event is missing from today: %d tokens", total.TotalTokens)
	}

	first, _, err := d.DayRange(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == "0001-01-01" {
		t.Fatal("history starts at year one; the day was derived before the fallback")
	}
}

// Invariant 1 for counters outside TotalTokens(): a longer total says nothing
// about them, and web search and fetch are billed per call.
func TestCountersOutsideTheTotalAreNeverLowered(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	rich := ev("r", 0, schema.CostBilled)
	rich.Usage = schema.Usage{
		InputTokens: 1_000, OutputTokens: 5_000,
		ReasoningTokens: 4_000, WebSearchCalls: 3, WebFetchCalls: 2,
	}
	ingest(t, d, rich)

	// A longer reading that simply did not parse the extras.
	poorer := ev("r", 0, schema.CostBilled)
	poorer.Usage = schema.Usage{InputTokens: 1_000, OutputTokens: 6_000}
	ingest(t, d, poorer)

	total, err := d.Totals(ctx, Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if total.ReasoningTokens != 4_000 {
		t.Errorf("reasoning_tokens = %d, want 4000 (it must never decrease)",
			total.ReasoningTokens)
	}
	if total.TotalTokens != 7_000 {
		t.Errorf("total_tokens = %d, want 7000", total.TotalTokens)
	}
}

// A shorter reading's native cost would price the larger stored count at a
// fraction of the truth.
func TestANativeCostFromAShorterReadingIsIgnored(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	ingest(t, d, ev("c", 2_000_000, schema.CostBilled))
	full, _ := d.Totals(ctx, Window{From: "2000-01-01"})
	if full.BilledUSD <= 0 {
		t.Fatal("no cost was recorded for the full reading")
	}

	tiny := 0.0001
	short := ev("c", 10, schema.CostBilled)
	short.NativeCostUSD = &tiny
	ingest(t, d, short)

	after, err := d.Totals(ctx, Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalTokens != 2_000_000 {
		t.Fatalf("token count changed: %d", after.TotalTokens)
	}
	if after.BilledUSD != full.BilledUSD {
		t.Fatalf("a 10-token reading rewrote the cost of a 2M-token one: %v -> %v",
			full.BilledUSD, after.BilledUSD)
	}
}

// A Down that loses event_daily fails every aggregate query, and a drop
// without IF EXISTS then wedges the Up.
func TestMigrationsRollBackAndForwardAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}

	viewExists := func(when string) {
		t.Helper()
		var n int
		if err := d.write.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='event_daily'`,
		).Scan(&n); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if n != 1 {
			t.Fatalf("%s: event_daily is missing; every aggregate query would fail", when)
		}
	}

	viewExists("after up")
	for i := range 3 {
		if err := goose.Down(d.write, "migrations"); err != nil {
			t.Fatalf("down %d: %v", i+1, err)
		}
	}
	if err := goose.Up(d.write, "migrations"); err != nil {
		t.Fatalf("rolling forward again: %v", err)
	}
	viewExists("after down and up")

	// And the schema still works end to end.
	if _, err := d.Totals(context.Background(), Window{From: "2000-01-01"}); err != nil {
		t.Fatalf("querying after a migration round trip: %v", err)
	}
}

// A laptop often carries a work login and a personal one; picking the
// dominant account must not drop the other's events from the count.
func TestAgentEventCountCoversEveryAccountOnTheMachine(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	work := ev("w1", 600, schema.CostBilled)
	personal := ev("p1", 500, schema.CostBilled)
	personal.ID, personal.AccountRef = "p1", "anthropic:b"
	second := ev("w2", 10, schema.CostBilled)

	if _, err := d.Ingest(ctx, &schema.Batch{
		V: schema.Version, MachineID: "m",
		Events: []schema.Event{work, personal, second},
		Accounts: []schema.Account{
			{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"},
			{Ref: "anthropic:b", Provider: "anthropic", Email: "dev@personal.example"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d agents, want 1", len(rows))
	}
	if rows[0].Events != 3 {
		t.Fatalf("events = %d, want 3 -- the machine's total, not one account's",
			rows[0].Events)
	}
	// The dominant account still names the person to go and talk to.
	if rows[0].Person != "dev@example.com" {
		t.Fatalf("person = %q, want the account with most events", rows[0].Person)
	}
}

// Counted from raw events alone, a machine syncing every five minutes reads
// as a nameless zero after the first prune.
func TestAgentRowSurvivesRetention(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	old := ev("old", 1_000, schema.CostBilled)
	old.ID, old.TS = "old-1", time.Now().AddDate(0, 0, -60)
	ingest(t, d, old)

	if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -30).UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d agents, want 1", len(rows))
	}
	if rows[0].Events != 1 {
		t.Errorf("events = %d, want 1 -- the rollup still holds it", rows[0].Events)
	}
	if rows[0].Person != "dev@example.com" {
		t.Errorf("person = %q, want it preserved through the rollup", rows[0].Person)
	}
}

// Ranked by the sum of the figures, mixed would lead; ranked by basis first,
// both billed sessions would outrank seat, the most expensive one.
func TestTopSessionsRankByCostNotByCostBasis(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// Native costs, so the amounts are exact.
	priced := func(id, session string, basis schema.CostBasis, usd float64) schema.Event {
		e := ev(id, 1_000, basis)
		e.ID, e.SessionID = id, session
		v := usd
		e.NativeCostUSD = &v
		return e
	}

	ingest(t,
		d,
		priced("seat-1", "seat", schema.CostRateCard, 100),
		priced("metered-1", "metered", schema.CostBilled, 1),
		// Largest sum (60 + 60 = 120) but not the largest session.
		priced("mixed-b", "mixed", schema.CostBilled, 60),
		priced("mixed-r", "mixed", schema.CostRateCard, 60),
	)

	rows, err := d.TopSessions(ctx, Window{From: "2000-01-01"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d sessions, want 3", len(rows))
	}

	got := []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}
	want := []string{"seat", "mixed", "metered"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v\n"+
				"  seat spent $100 and must rank first: ranking by basis would "+
				"bury it under a $1 metered session\n"+
				"  mixed totals $120 across both columns and must not rank first: "+
				"the two are never summed", got, want)
		}
	}

	// The columns themselves stay separate whatever the ordering does.
	if rows[1].BilledUSD != 60 || rows[1].RateCardUSD != 60 {
		t.Fatalf("mixed reported billed=%v ratecard=%v, want 60 and 60 kept apart",
			rows[1].BilledUSD, rows[1].RateCardUSD)
	}
}

// event.agent_version is the harness's version; machine.agent_version is the
// collector's, a different value.
func TestHarnessVersionIsStored(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	e := ev("v", 100, schema.CostBilled)
	e.AgentVersion = "claude-code/2.1.4"
	ingest(t, d, e)

	var got string
	if err := d.read.QueryRowContext(ctx,
		`SELECT agent_version FROM event WHERE id = 'v'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "claude-code/2.1.4" {
		t.Fatalf("agent_version = %q, want the harness version", got)
	}

	// A later reading that does not know the version must not blank it.
	blank := ev("v", 200, schema.CostBilled)
	ingest(t, d, blank)
	if err := d.read.QueryRowContext(ctx,
		`SELECT agent_version FROM event WHERE id = 'v'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "claude-code/2.1.4" {
		t.Fatalf("agent_version was blanked by a later reading: %q", got)
	}
}

// An hour that ended at 91% must not report the 8% it started at.
func TestQuotaKeepsThePeakNotTheFirstReading(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	sample := func(pct float64) schema.QuotaSample {
		return schema.QuotaSample{
			V: schema.Version, ID: "q-hour-1", Source: schema.SourceCodex,
			TS: time.Now(), MachineID: "m", AccountRef: "anthropic:a",
			WindowMinutes: 300, UsedPercent: pct, PlanType: "max",
		}
	}
	push := func(pct float64) {
		t.Helper()
		if _, err := d.Ingest(ctx, &schema.Batch{
			V: schema.Version, MachineID: "m",
			Quota: []schema.QuotaSample{sample(pct)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	push(8)
	push(91)
	// And a later dip must not pull it back down.
	push(12)

	rows := quotaRows(t, d)
	if len(rows) != 1 {
		t.Fatalf("got %d quota rows, want 1", len(rows))
	}
	if rows[0].usedPercent != 91 {
		t.Fatalf("used_percent = %v, want 91 -- the peak, not the first or last reading",
			rows[0].usedPercent)
	}
}

// quotaRow is one stored quota sample, as ingest left it.
type quotaRow struct {
	id, limitID, limitName string
	usedPercent            float64
	resetsAt               int64
}

func quotaRows(t *testing.T, d *DB) []quotaRow {
	t.Helper()
	rows, err := d.read.QueryContext(context.Background(),
		`SELECT id, limit_id, limit_name, used_percent, resets_at FROM quota_sample ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []quotaRow
	for rows.Next() {
		var r quotaRow
		if err := rows.Scan(&r.id, &r.limitID, &r.limitName, &r.usedPercent, &r.resetsAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Codex reports a general "codex" pool beside a per-model one; each keeps its
// own peak and name.
func TestQuotaPoolsAreStoredApart(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	now := time.Now()
	sample := func(limitID, name string, pct float64, at time.Time) schema.QuotaSample {
		return schema.QuotaSample{
			V: schema.Version, ID: "q:" + limitID, Source: schema.SourceCodex,
			TS: at, MachineID: "m", AccountRef: "openai:a", PlanType: "pro",
			LimitID: limitID, LimitName: name,
			WindowMinutes: 10080, UsedPercent: pct,
			ResetsAt: now.Add(48 * time.Hour),
		}
	}
	if _, err := d.Ingest(ctx, &schema.Batch{
		V: schema.Version, MachineID: "m",
		Quota: []schema.QuotaSample{
			sample("codex", "", 91, now.Add(-2*time.Hour)),
			sample("codex_bengalfox", "GPT-5.3-Codex-Spark", 0, now.Add(-time.Hour)),
		},
	}); err != nil {
		t.Fatal(err)
	}

	rows := quotaRows(t, d)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per pool", len(rows))
	}
	if rows[0].limitID != "codex" || rows[0].usedPercent != 91 {
		t.Fatalf("codex pool stored as %q at %v%%, want 91%% -- an untouched pool "+
			"must not stand in for a nearly exhausted one", rows[0].limitID, rows[0].usedPercent)
	}
	if rows[1].limitName != "GPT-5.3-Codex-Spark" {
		t.Fatalf("second pool stored as %q, want its own name", rows[1].limitName)
	}
}

// Unclamped, the heatmap reports a fraction of the totals beside it after a
// prune.
func TestHeatmapDoesNotSilentlyDisagreeWithTotals(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	old := ev("old", 1_000, schema.CostBilled)
	old.ID, old.TS = "heat-old", time.Now().AddDate(0, 0, -60)
	recent := ev("new", 500, schema.CostBilled)
	recent.ID = "heat-new"
	ingest(t, d, old, recent)

	if _, err := d.Prune(ctx, time.Now().AddDate(0, 0, -30).UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	w := Window{From: "2000-01-01"}
	cells, _, err := d.Heatmap(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	var heat int64
	for _, c := range cells {
		heat += c.Tokens
	}

	floor, err := d.RetentionFloor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if floor == "" {
		t.Fatal("no retention floor was recorded")
	}
	// Within the window the heatmap can actually cover, it must agree with
	// the totals for that same window.
	clamped, err := d.Totals(ctx, Window{From: floor})
	if err != nil {
		t.Fatal(err)
	}
	if heat != clamped.TotalTokens {
		t.Fatalf("heatmap has %d tokens, totals over the same window have %d",
			heat, clamped.TotalTokens)
	}
}

// Every SET in the upsert needs an arm in its WHERE, or a sample whose only
// news is a later reset time changes nothing.
func TestAQuotaResetTimeMovesOnItsOwn(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	first := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	later := first.Add(5 * time.Hour)
	for _, resets := range []time.Time{first, later} {
		if _, err := d.Ingest(ctx, &schema.Batch{
			V: schema.Version, MachineID: "m",
			Quota: []schema.QuotaSample{{
				V: schema.Version, ID: "q", Source: schema.SourceCodex, TS: time.Now(),
				MachineID: "m", AccountRef: "openai:a", LimitID: "codex",
				WindowMinutes: 300, UsedPercent: 40, ResetsAt: resets,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if rows := quotaRows(t, d); len(rows) != 1 || rows[0].resetsAt != later.Unix() {
		t.Fatalf("got %+v, want the later reset time %d", rows, later.Unix())
	}
}

// An agent that could not read its hostname sends none.
func TestAnEmptyHostnameNeverBlanksAMachine(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	host := func(name string) string {
		t.Helper()
		if _, err := d.Ingest(ctx, &schema.Batch{
			V: schema.Version, MachineID: "m", Hostname: name, AgentVersion: "v1.4.0",
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := d.Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d agents, want 1", len(rows))
		}
		return rows[0].Hostname
	}
	host("laptop")
	if got := host(""); got != "laptop" {
		t.Fatalf("hostname = %q after a push without one, want %q kept", got, "laptop")
	}
	if got := host("renamed"); got != "renamed" {
		t.Fatalf("hostname = %q, want a new name taken", got)
	}
}
