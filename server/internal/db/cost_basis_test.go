package db

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// Priced usage with an unknown basis -- an API key, every Copilot event -- is
// a third figure, beside the other two and never summed with them.
func TestUnknownBasisSpendIsReportedApart(t *testing.T) {
	d := newDB(t)
	ingest(t, d,
		ev("billed", 1_000_000, schema.CostBilled),
		ev("seat", 2_000_000, schema.CostRateCard),
		ev("apikey", 3_000_000, schema.CostUnknown),
	)
	price := func(tok int64) float64 {
		e := ev("p", tok, schema.CostBilled)
		usd, ok := d.prices.Cost(&e)
		if !ok {
			t.Fatal("setup: the fixture model is unpriced")
		}
		return usd
	}

	tot, err := d.Totals(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if tot.BilledUSD != price(1_000_000) || tot.RateCardUSD != price(2_000_000) ||
		tot.UnknownBasisUSD != price(3_000_000) {
		t.Fatalf("billed=%v rate_card=%v unknown_basis=%v; each must hold exactly its own "+
			"usage (%v, %v, %v)", tot.BilledUSD, tot.RateCardUSD, tot.UnknownBasisUSD,
			price(1_000_000), price(2_000_000), price(3_000_000))
	}
	if tot.UnknownBasisTokens != 3_000_000 || tot.UnpricedTokens != 0 {
		t.Fatalf("unknown_basis_tokens=%d unpriced_tokens=%d, want 3000000 and 0",
			tot.UnknownBasisTokens, tot.UnpricedTokens)
	}
}

// The effective billed rate divides billed dollars by the tokens that earned
// them. Divided by every token, seat usage and unpriced models drag it toward
// zero.
func TestBilledTokensCountOnlyPricedBilledUsage(t *testing.T) {
	d := newDB(t)
	unpriced := ev("mystery", 5_000, schema.CostBilled)
	unpriced.Model = "not-a-real-model"
	ingest(t, d,
		ev("billed", 1_000, schema.CostBilled),
		unpriced,
		ev("seat", 7_000, schema.CostRateCard),
	)
	tot, err := d.Totals(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if tot.BilledTokens != 1_000 {
		t.Fatalf("billed_tokens = %d, want 1000: only the priced billed usage", tot.BilledTokens)
	}
}

// A session driven on an API key is ranked by what it cost, like any other.
func TestSessionsCarryAndRankByUnknownBasisSpend(t *testing.T) {
	d := newDB(t)
	priced := func(id, session string, basis schema.CostBasis, usd float64) schema.Event {
		e := ev(id, 1_000, basis)
		e.SessionID = session
		e.NativeCostUSD = &usd
		return e
	}
	ingest(t, d,
		priced("k", "apikey", schema.CostUnknown, 50),
		priced("s", "seat", schema.CostRateCard, 10),
	)
	rows, err := d.TopSessions(context.Background(), Window{From: "2000-01-01"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SessionID != "apikey" || rows[0].UnknownBasisUSD != 50 {
		t.Fatalf("got %+v, want the $50 unknown-basis session first, carrying its spend", rows)
	}
	if rows[0].BilledUSD != 0 || rows[0].RateCardUSD != 0 {
		t.Fatalf("unknown-basis spend leaked into another figure: %+v", rows[0])
	}
}

func TestTopSessionsPublishNoProjectPath(t *testing.T) {
	typ := reflect.TypeFor[SessionRow]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if strings.Contains(name, "project") || strings.Contains(name, "path") {
			t.Fatalf("SessionRow.%s is published as %q", typ.Field(i).Name, name)
		}
	}
}

// The matrix reports the same three figures as everything else.
func TestMatrixCellsCarryUnknownBasisSpend(t *testing.T) {
	d := newDB(t)
	usd := 4.0
	e := ev("k", 1_000, schema.CostUnknown)
	e.NativeCostUSD = &usd
	ingest(t, d, e)
	m, err := d.Matrix(context.Background(), Window{From: "2000-01-01"}, "model", "effort", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cells) != 1 || m.Cells[0].UnknownBasisUSD != usd ||
		m.Cells[0].BilledUSD != 0 || m.Cells[0].RateCardUSD != 0 {
		t.Fatalf("got %+v, want one cell holding $%v of unknown-basis spend alone", m.Cells, usd)
	}
}

// Unmarked, an unrecognised model reads as $0 of billed spend.
func TestTheExportMarksUnpricedTokens(t *testing.T) {
	d := newDB(t)
	e := ev("mystery", 5_000_000, schema.CostBilled)
	e.Model = "not-a-real-model"
	ingest(t, d, e, ev("known", 1_000, schema.CostBilled))

	rows, err := d.Export(context.Background(), Window{From: "2000-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		want := int64(0)
		if r.Model == "not-a-real-model" {
			want = r.Tokens
		}
		if r.UnpricedTokens != want {
			t.Fatalf("%s: unpriced_tokens = %d, want %d", r.Model, r.UnpricedTokens, want)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}
