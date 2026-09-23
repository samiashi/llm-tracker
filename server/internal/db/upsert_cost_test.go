package db

import (
	"context"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// storedEvent reads back everything pricing sees of a stored row, plus its
// cost. Listed out rather than taken from pricedColumns, so a column missing
// there is caught here rather than agreed with.
func storedEvent(t *testing.T, d *DB, id string) (e schema.Event, cost float64, source string) {
	t.Helper()
	u := &e.Usage
	if err := d.read.QueryRowContext(context.Background(), `
		SELECT cost_usd, cost_source, source, model, endpoint, speed, inference_geo,
		       input_tokens, output_tokens, cache_read_tokens, cache_write_5m,
		       cache_write_1h, web_search_calls, web_fetch_calls
		FROM event WHERE id = ?`, id).Scan(&cost, &source,
		(*string)(&e.Source), &e.Model, &e.Endpoint, &e.Speed, &e.InferenceGeo,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWrite5mTokens,
		&u.CacheWrite1hTokens, &u.WebSearchCalls, &u.WebFetchCalls); err != nil {
		t.Fatal(err)
	}
	return e, cost, source
}

// Whatever order readings arrive in, a table-priced row costs what its stored
// columns cost, and a native figure gives way only to another native one from
// a reading at least as long.
func TestStoredCostPricesTheStoredRow(t *testing.T) {
	native := func(v float64) *float64 { return &v }
	reading := func(in, out, cacheRead, searches int64, cost *float64) schema.Event {
		e := ev("x", 0, schema.CostBilled)
		e.Usage = schema.Usage{InputTokens: in, OutputTokens: out,
			CacheReadTokens: cacheRead, WebSearchCalls: searches}
		e.NativeCostUSD = cost
		return e
	}
	cached := reading(1_000, 0, 0, 0, nil)
	cached.Usage.CacheWrite5mTokens, cached.Usage.CacheWrite1hTokens = 200_000, 300_000
	cached.Speed, cached.InferenceGeo = "fast", "us"

	cases := []struct {
		name       string
		readings   []schema.Event
		wantNative *float64
	}{
		{"a longer reading reprices", []schema.Event{
			reading(1_000, 0, 0, 0, nil), reading(2_000, 0, 0, 0, nil)}, nil},
		{"a shorter reading changes nothing", []schema.Event{
			reading(2_000, 0, 0, 0, nil), reading(1_000, 0, 0, 0, nil)}, nil},
		{"more searches at an equal total are charged", []schema.Event{
			reading(1_000, 0, 0, 0, nil), reading(1_000, 0, 0, 10, nil)}, nil},
		{"searches kept from a shorter reading are charged", []schema.Event{
			reading(2_000, 0, 0, 0, nil), reading(1_000, 0, 0, 10, nil)}, nil},
		{"a longer reading with fewer searches keeps their charge", []schema.Event{
			reading(1_000, 5_000, 0, 3, nil), reading(1_000, 5_100, 0, 0, nil)}, nil},
		{"an equal re-split with a search prices the split that is stored", []schema.Event{
			reading(10_000, 0, 0, 0, nil), reading(0, 0, 10_000, 1, nil)}, nil},
		{"an equal native figure replaces a table estimate", []schema.Event{
			reading(1_000, 0, 0, 0, nil), reading(1_000, 0, 0, 0, native(7))}, native(7)},
		{"a shorter native figure is ignored", []schema.Event{
			reading(2_000, 0, 0, 0, nil), reading(1_000, 0, 0, 0, native(7))}, nil},
		{"a longer native figure replaces a shorter one", []schema.Event{
			reading(1_000, 0, 0, 0, native(5)), reading(2_000, 0, 0, 0, native(9))}, native(9)},
		{"a table reading with a search never replaces a native figure", []schema.Event{
			reading(100_000, 0, 0, 0, native(5)), reading(100_000, 0, 0, 1, nil)}, native(5)},
		{"a longer table reading never replaces a native figure", []schema.Event{
			reading(1_000, 0, 0, 0, native(5)), reading(2_000, 0, 0, 0, nil)}, native(5)},
		{"every pricing column of a kept split is charged", []schema.Event{
			cached, reading(1_000, 0, 0, 7, nil)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDB(t)
			var longest int64
			for _, r := range c.readings {
				ingest(t, d, r)
				longest = max(longest, r.Usage.TotalTokens())
			}
			stored, cost, source := storedEvent(t, d, "x")

			if got := stored.Usage.TotalTokens(); got != longest {
				t.Fatalf("stored total %d, want the longest reading's %d", got, longest)
			}
			if c.wantNative != nil {
				if source != "native" || cost != *c.wantNative {
					t.Fatalf("stored (%v, %s), want the native %v", cost, source, *c.wantNative)
				}
				return
			}
			want, wantSource := d.priceEvent(&stored)
			if cost != want || source != wantSource {
				t.Fatalf("stored (%v, %s), but the stored row prices at (%v, %s)",
					cost, source, want, wantSource)
			}
		})
	}
}

// 'unknown' is the absence of a basis: a reading that knows fills it in, even
// at equal length, and nothing blanks a known one.
func TestAKnownCostBasisFillsInAndIsNeverBlanked(t *testing.T) {
	basis := func(d *DB) string {
		t.Helper()
		var b string
		if err := d.read.QueryRowContext(context.Background(),
			`SELECT cost_basis FROM event WHERE id = 'b'`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	with := func(tok int64, b schema.CostBasis, collector int) schema.Event {
		e := ev("b", tok, b)
		e.Collector = collector
		return e
	}

	cases := []struct {
		name             string
		stored, incoming schema.Event
		want             schema.CostBasis
	}{
		{"a resolved basis fills in unknown",
			with(1_000, schema.CostUnknown, 8), with(1_000, schema.CostRateCard, 8), schema.CostRateCard},
		{"unknown never blanks a known basis, even from a longer reading",
			with(1_000, schema.CostRateCard, 8), with(2_000, schema.CostUnknown, 8), schema.CostRateCard},
		{"unknown never blanks a known basis, even from a newer collector",
			with(1_000, schema.CostBilled, 8), with(1_000, schema.CostUnknown, 9), schema.CostBilled},
		{"a newer collector corrects a known basis",
			with(1_000, schema.CostBilled, 8), with(1_000, schema.CostRateCard, 9), schema.CostRateCard},
		{"the same collector does not flip a known basis",
			with(1_000, schema.CostBilled, 8), with(2_000, schema.CostRateCard, 8), schema.CostBilled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDB(t)
			ingest(t, d, c.stored)
			ingest(t, d, c.incoming)
			if got := basis(d); got != string(c.want) {
				t.Fatalf("cost_basis = %q, want %q", got, c.want)
			}
		})
	}
}

// A CollectorVersion bump is how a corrected adapter reaches rows already
// uploaded; an old agent, which sends collector 0, must never undo it.
func TestANewerCollectorCorrectsTheStoredReading(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	at := time.Now().Add(-48 * time.Hour)

	wrong := ev("c", 1_000_000, schema.CostRateCard)
	wrong.Collector, wrong.TS = 7, at
	wrong.Model, wrong.Effort, wrong.Speed = "claude-sonnet-5", "high", ""
	wrong.SessionID = "s-wrong"
	ingest(t, d, wrong)

	fixed := wrong
	fixed.Collector, fixed.TS = 8, at.Add(-24*time.Hour)
	fixed.Model, fixed.Effort = "claude-opus-5", "xhigh"
	fixed.SessionID = "s-right"
	fixed.IsSubagent = true
	if res := ingest(t, d, fixed); res.EventsStored != 1 {
		t.Fatalf("stored %d: an equal reading from a newer collector must be taken", res.EventsStored)
	}

	type row struct {
		model, effort, session, day string
		subagent, collector         int
	}
	read := func() row {
		t.Helper()
		var r row
		if err := d.read.QueryRowContext(ctx, `
			SELECT model, effort, session_id, day, is_subagent, collector
			FROM event WHERE id = 'c'`).Scan(&r.model, &r.effort, &r.session,
			&r.day, &r.subagent, &r.collector); err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := row{"claude-opus-5", "xhigh", "s-right", fixed.TS.UTC().Format("2006-01-02"), 1, 8}
	if got := read(); got != want {
		t.Fatalf("stored %+v, want the newer collector's reading %+v", got, want)
	}
	stored, cost, source := storedEvent(t, d, "c")
	if wantCost, wantSource := d.priceEvent(&stored); cost != wantCost || source != wantSource {
		t.Fatalf("cost (%v, %s) was not repriced for the corrected model: want (%v, %s)",
			cost, source, wantCost, wantSource)
	}

	// An older collector's reading, even a longer one, moves the tokens and
	// nothing the newer one recorded.
	stale := wrong
	stale.Collector = 0
	stale.Usage.InputTokens = 2_000_000
	ingest(t, d, stale)
	if got := read(); got != want {
		t.Fatalf("an older collector overrode a newer reading: stored %+v", got)
	}
	stored, cost, _ = storedEvent(t, d, "c")
	if stored.Usage.InputTokens != 2_000_000 {
		t.Fatalf("input = %d, want the longer reading's tokens", stored.Usage.InputTokens)
	}
	if wantCost, _ := d.priceEvent(&stored); cost != wantCost {
		t.Fatalf("cost %v does not price the stored row (%v)", cost, wantCost)
	}

	// And a newer collector's shorter reading never lowers the total.
	short := fixed
	short.Collector = 9
	short.Usage.InputTokens = 10
	ingest(t, d, short)
	if stored, _, _ := storedEvent(t, d, "c"); stored.Usage.TotalTokens() != 2_000_000 {
		t.Fatalf("total fell to %d on a shorter reading", stored.Usage.TotalTokens())
	}
}

// Readings merged within one batch are priced like readings in separate ones;
// only an insert, being the reading itself, keeps the price it arrived with.
func TestAMergeMidBatchPricesTheStoredRow(t *testing.T) {
	d := newDB(t)
	reading := func(id string, in, searches int64) schema.Event {
		e := ev(id, in, schema.CostBilled)
		e.Usage.WebSearchCalls = searches
		return e
	}
	ingest(t, d, reading("old", 2_000, 0))

	// New rows around merges into an older one and into one inserted here.
	ingest(t, d,
		reading("new-1", 1_000, 0),
		reading("old", 1_000, 10),
		reading("new-2", 1_000, 0),
		reading("new-2", 500, 4),
		reading("new-3", 1_000, 0),
	)
	for _, id := range []string{"old", "new-1", "new-2", "new-3"} {
		stored, cost, source := storedEvent(t, d, id)
		if want, wantSource := d.priceEvent(&stored); cost != want || source != wantSource {
			t.Fatalf("%s stored (%v, %s), but the stored row prices at (%v, %s)",
				id, cost, source, want, wantSource)
		}
	}
}
