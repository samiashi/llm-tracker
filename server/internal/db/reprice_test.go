package db

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// Reprice runs beside a live server: written back unconditionally, a merged
// reading would get the stale price and a native figure a table estimate.
func TestRepriceNeverOverwritesAConcurrentIngest(t *testing.T) {
	if testing.Short() {
		t.Skip("ingests 60,000 events")
	}
	d := newDB(t)
	ctx := context.Background()

	const n = 60_000
	base := make([]schema.Event, n)
	for i := range base {
		base[i] = ev(fmt.Sprintf("rp-%d", i), 1_000, schema.CostBilled)
	}
	for i := 0; i < n; i += 2_000 {
		ingest(t, d, base[i:i+2_000]...)
	}
	// A new release's rate, so every row is stale and reprice writes
	// throughout; against an unchanged table it writes nothing and races
	// nothing. Set before anything reads the table concurrently.
	r := d.prices.Rates["|claude-opus-5"]
	r.Input *= 2
	d.prices.Rates["|claude-opus-5"] = r

	// Meanwhile: longer readings of half the rows, native figures for the rest.
	var stop atomic.Bool
	var native sync.Map
	var wg sync.WaitGroup
	wg.Go(func() {
		for round := int64(1); !stop.Load(); round++ {
			for i := 0; i < n && !stop.Load(); i += 500 {
				batch := make([]schema.Event, 0, 500)
				for j := i; j < i+500; j++ {
					e := base[j]
					if j%2 == 0 {
						e.Usage.InputTokens = 1_000 + round
					} else {
						usd := 0.123
						e.NativeCostUSD = &usd
					}
					batch = append(batch, e)
				}
				if _, err := d.Ingest(ctx, testLogin, &schema.Batch{
					V: schema.Version, MachineID: "m", Events: batch,
				}); err != nil {
					t.Error(err)
					return
				}
				for _, e := range batch {
					if e.NativeCostUSD != nil {
						native.Store(e.ID, true)
					}
				}
			}
		}
	})
	wrote, err := d.reprice(ctx)
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if wrote == 0 {
		t.Fatal("setup: reprice wrote nothing, so it raced nothing")
	}

	stale, clobbered := 0, 0
	for i := range base {
		stored, cost, source := storedEvent(t, d, base[i].ID)
		if _, ok := native.Load(base[i].ID); ok && source != "native" {
			clobbered++
		}
		if source == "native" {
			continue
		}
		if want, _ := d.priceEvent(&stored); cost != want {
			stale++
		}
	}
	if stale > 0 || clobbered > 0 {
		t.Fatalf("reprice raced ingest: %d rows priced from a stale reading, "+
			"%d native costs replaced by table estimates", stale, clobbered)
	}
}

// The write is conditional on what was priced, so a snapshot that ingest has
// since overtaken changes nothing.
func TestRepriceWritesOnlyWhatItPriced(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	snapshot := func(id string) repricing {
		stored, cost, source := storedEvent(t, d, id)
		return repricing{id: id, cost: cost + 1, source: source, priced: stored}
	}
	ingest(t, d,
		ev("longer", 1_000, schema.CostBilled),
		ev("native", 1_000, schema.CostBilled),
		ev("still", 1_000, schema.CostBilled))
	longer, native, still := snapshot("longer"), snapshot("native"), snapshot("still")

	grown := ev("longer", 2_000, schema.CostBilled)
	usd := 0.5
	harness := ev("native", 1_000, schema.CostBilled)
	harness.NativeCostUSD = &usd
	ingest(t, d, grown, harness)

	n, err := d.applyReprice(ctx, []repricing{longer, native, still})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("wrote %d rows, want only the one still holding what was priced", n)
	}
	if stored, cost, _ := storedEvent(t, d, "longer"); cost != func() float64 {
		c, _ := d.priceEvent(&stored)
		return c
	}() {
		t.Fatalf("a stale snapshot overwrote the longer reading's price: %v", cost)
	}
	if _, cost, source := storedEvent(t, d, "native"); source != "native" || cost != usd {
		t.Fatalf("native figure replaced: (%v, %s)", cost, source)
	}
	if _, cost, _ := storedEvent(t, d, "still"); cost != still.cost {
		t.Fatalf("an unchanged row was not repriced: %v, want %v", cost, still.cost)
	}
}

// A corrected rate reaches stored rows, and only rows whose price changed are
// written.
func TestRepriceAppliesACorrectedRate(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	usd := 3.0
	harness := ev("native", 1_000_000, schema.CostBilled)
	harness.NativeCostUSD = &usd
	ingest(t, d, ev("a", 1_000_000, schema.CostBilled), ev("b", 2_000_000, schema.CostBilled), harness)

	if n, err := d.reprice(ctx); err != nil || n != 0 {
		t.Fatalf("repriced %d rows (%v) against an unchanged table, want 0", n, err)
	}

	r := d.prices.Rates["|claude-opus-5"]
	r.Input *= 2
	d.prices.Rates["|claude-opus-5"] = r
	n, err := d.reprice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("repriced %d rows, want the 2 table-priced ones", n)
	}
	for _, id := range []string{"a", "b"} {
		stored, cost, _ := storedEvent(t, d, id)
		if want, _ := d.priceEvent(&stored); cost != want {
			t.Fatalf("%s costs %v, want the corrected %v", id, cost, want)
		}
	}
	if _, cost, source := storedEvent(t, d, "native"); source != "native" || cost != usd {
		t.Fatalf("reprice touched a native figure: (%v, %s)", cost, source)
	}
}

// A release is the only thing that changes prices, so each new build reprices
// once and a restart of the same build does nothing.
func TestEachNewBuildRepricesOnce(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, ev("a", 1_000_000, schema.CostBilled))
	cost := func() float64 {
		_, c, _ := storedEvent(t, d, "a")
		return c
	}
	before := cost()

	// What a new release's table does, without its version changing.
	r := d.prices.Rates["|claude-opus-5"]
	r.Input *= 2
	d.prices.Rates["|claude-opus-5"] = r

	if n, ran, err := d.RepriceIfChanged(ctx, "v1.4.0"); err != nil || !ran || n != 1 {
		t.Fatalf("first start of v1.4.0: repriced %d, ran %v, err %v; want the one row", n, ran, err)
	}
	if cost() == before {
		t.Fatal("the stored cost did not follow the new rate")
	}
	if _, ran, err := d.RepriceIfChanged(ctx, "v1.4.0"); err != nil || ran {
		t.Fatalf("a restart of the same build repriced again (err %v)", err)
	}
	if n, ran, err := d.RepriceIfChanged(ctx, "v1.5.0"); err != nil || !ran || n != 0 {
		t.Fatalf("v1.5.0: repriced %d, ran %v, err %v; want a pass that changes nothing", n, ran, err)
	}
}

// A working tree's build string survives `make prices`, and the table's
// version only counts its keys: named by those alone, a changed rate would
// never reach the rows already stored, and totals would mix two tables.
func TestAChangedRateRepricesUnderTheSameBuild(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ingest(t, d, ev("a", 1_000_000, schema.CostBilled))
	if _, ran, err := d.RepriceIfChanged(ctx, "176d913-dirty"); err != nil || !ran {
		t.Fatalf("first start: ran %v (%v)", ran, err)
	}

	r := d.prices.Rates["|claude-opus-5"]
	r.Input *= 2
	d.prices.Rates["|claude-opus-5"] = r
	if n, ran, err := d.RepriceIfChanged(ctx, "176d913-dirty"); err != nil || !ran || n != 1 {
		t.Fatalf("after a rate changed: repriced %d, ran %v (%v); want the one row", n, ran, err)
	}
	stored, cost, _ := storedEvent(t, d, "a")
	if want, _ := d.priceEvent(&stored); cost != want {
		t.Fatalf("stored cost %v, the table now prices it at %v", cost, want)
	}
}

// A release that learns a model's price is zero -- a local runtime added to
// the list -- changes no cost, only where it came from. Compared on cost
// alone, the row stays unpriced and the dashboard keeps flagging it for good.
func TestRepriceMovesAZeroCostRowOutOfUnpriced(t *testing.T) {
	d := newDB(t)
	e := ev("local", 1_000, schema.CostBilled)
	e.Model = "not-a-real-model"
	ingest(t, d, e)
	if _, _, source := storedEvent(t, d, "local"); source != "unpriced" {
		t.Fatalf("setup: stored as %s", source)
	}
	d.prices.Rates[schema.PriceKey("", "not-a-real-model")] = schema.Rate{}
	if _, err := d.reprice(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, cost, source := storedEvent(t, d, "local"); source != "table" || cost != 0 {
		t.Fatalf("stored (%v, %s) after the table priced it at $0, want (0, table)", cost, source)
	}
}

// Tiers depend on the event's source: a stored row priced without it falls
// back to the base rate, half the cost of a 300k-token prompt.
func TestALongContextRequestKeepsItsTierPriceWhenRepriced(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	e := ev("long", 0, schema.CostBilled)
	e.Source, e.Model = schema.SourceCodex, "gpt-5.4"
	e.Usage = schema.Usage{InputTokens: 300_000, OutputTokens: 1_000}
	want, _ := d.priceEvent(&e)
	base := schema.DefaultPriceTable().Rates["|gpt-5.4"]
	if flat := (300_000*base.Input + 1_000*base.Output) / 1e6; want <= flat {
		t.Fatalf("setup: %v is not above the base-rate %v; gpt-5.4 has no tier to test", want, flat)
	}

	ingest(t, d, e)
	e.Usage.OutputTokens++ // a longer reading: the merge path reprices the stored row
	want, _ = d.priceEvent(&e)
	ingest(t, d, e)
	if _, cost, _ := storedEvent(t, d, "long"); cost != want {
		t.Fatalf("after a merge the row costs %v, want the tier price %v", cost, want)
	}
	if n, err := d.reprice(ctx); err != nil || n != 0 {
		t.Fatalf("reprice changed %d rows (%v) against an unchanged table, want 0", n, err)
	}
	if _, cost, _ := storedEvent(t, d, "long"); cost != want {
		t.Fatalf("after reprice the row costs %v, want the tier price %v", cost, want)
	}
}
