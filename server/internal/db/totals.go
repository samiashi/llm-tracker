package db

import (
	"context"
	"time"
)

// Totals and the three spend figures, over event_daily (invariant 5), kept
// apart by construction (invariant 3).

// Totals is the headline figure set.
//
// Billed, rate-card and unknown-basis spend are three fields, never a sum:
// one is money that left the bank, one is what seat usage would have cost on
// the API, and the third is priced usage whose adapter could not say which --
// an API-key session, Copilot. Unpriced volume is reported so it stays
// visible instead of disappearing into a zero.
type Totals struct {
	Events           int64   `json:"events"`
	TotalTokens      int64   `json:"total_tokens"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	BilledUSD        float64 `json:"billed_usd"`
	// BilledTokens is the priced usage behind BilledUSD, the only divisor
	// that gives a per-token rate for it.
	BilledTokens       int64   `json:"billed_tokens"`
	RateCardUSD        float64 `json:"rate_card_usd"`
	UnknownBasisUSD    float64 `json:"unknown_basis_usd"`
	UnknownBasisTokens int64   `json:"unknown_basis_tokens"`
	UnpricedTokens     int64   `json:"unpriced_tokens"`
	UnpricedEvents     int64   `json:"unpriced_events"`
}

// totalsSelect runs against event_daily, where a row is already an aggregate,
// so the event count is a column rather than COUNT(*).
const totalsSelect = `
  COALESCE(SUM(events),0),
  COALESCE(SUM(total_tokens),0),
  COALESCE(SUM(input_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cache_write_5m + cache_write_1h),0),
  COALESCE(SUM(reasoning_tokens),0),
  ` + billedUSD + `,
  COALESCE(SUM(CASE WHEN cost_basis='billed' AND cost_source!='unpriced' THEN total_tokens ELSE 0 END),0),
  ` + rateCardUSD + `,
  ` + unknownBasisUSD + `,
  COALESCE(SUM(CASE WHEN cost_basis NOT IN ` + knownBasis + ` AND cost_source!='unpriced' THEN total_tokens ELSE 0 END),0),
  ` + unpricedUsage + `,
  COALESCE(SUM(CASE WHEN cost_source='unpriced' THEN events ELSE 0 END),0)`

// knownBasis is the cost bases with a money figure of their own. Every other
// value is reported as unknown basis, so priced spend lands in exactly one of
// the three.
const knownBasis = `('billed','rate_card_equivalent')`

// The three spend columns every query reports. Separate by construction:
// invariant 3 forbids summing them.
const (
	billedUSD       = `COALESCE(SUM(CASE WHEN cost_basis='billed' THEN cost_usd ELSE 0 END),0)`
	rateCardUSD     = `COALESCE(SUM(CASE WHEN cost_basis='rate_card_equivalent' THEN cost_usd ELSE 0 END),0)`
	unknownBasisUSD = `COALESCE(SUM(CASE WHEN cost_basis NOT IN ` + knownBasis + ` THEN cost_usd ELSE 0 END),0)`
)

// unpricedUsage is the usage no rate matched, reported beside every cost so a
// $0 cannot pass for free usage (invariant 8).
const unpricedUsage = `COALESCE(SUM(CASE WHEN cost_source='unpriced' THEN total_tokens ELSE 0 END),0)`

// totalsDest returns scan destinations in totalsSelect's column order, so the
// order is written once. The three spend fields are all float64: a swap in a
// hand-copied list would compile and scan without error.
func totalsDest(t *Totals) []any {
	return []any{&t.Events, &t.TotalTokens, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens, &t.ReasoningTokens,
		&t.BilledUSD, &t.BilledTokens, &t.RateCardUSD,
		&t.UnknownBasisUSD, &t.UnknownBasisTokens,
		&t.UnpricedTokens, &t.UnpricedEvents}
}

func scanTotals(row interface{ Scan(...any) error }) (Totals, error) {
	var t Totals
	err := row.Scan(totalsDest(&t)...)
	return t, err
}

func (d *DB) Totals(ctx context.Context, w Window) (Totals, error) {
	w = w.Normalise()
	where, args := w.where("")
	row := d.read.QueryRowContext(ctx, `SELECT `+totalsSelect+` FROM event_daily WHERE `+where, args...)
	return scanTotals(row)
}

// Daily is one day of the time series.
type Daily struct {
	Day    string `json:"day"`
	Totals Totals `json:"totals"`
}

func (d *DB) Daily(ctx context.Context, w Window) ([]Daily, error) {
	w = w.Normalise()
	where, args := w.where("")
	rows, err := d.read.QueryContext(ctx,
		`SELECT day, `+totalsSelect+` FROM event_daily WHERE `+where+` GROUP BY day ORDER BY day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Daily, 0)
	for rows.Next() {
		var x Daily
		if err := rows.Scan(append([]any{&x.Day}, totalsDest(&x.Totals)...)...); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Compare is the totals of the equally-sized window immediately before the
// one asked for. The window's own totals are the summary's.
type Compare struct {
	Previous Totals `json:"previous"`
	// PreviousFrom and PreviousTo name the comparison window, so the
	// dashboard can say what it compares against.
	PreviousFrom string `json:"previous_from"`
	PreviousTo   string `json:"previous_to"`
}

func (d *DB) Compare(ctx context.Context, w Window) (Compare, error) {
	w = w.Normalise()
	from, err := time.Parse("2006-01-02", w.From)
	if err != nil {
		return Compare{}, err
	}
	to, err := time.Parse("2006-01-02", w.To)
	if err != nil {
		return Compare{}, err
	}
	days := int(to.Sub(from).Hours()/24) + 1

	prev := Window{
		From:   from.AddDate(0, 0, -days).Format("2006-01-02"),
		To:     from.AddDate(0, 0, -1).Format("2006-01-02"),
		Person: w.Person,
	}
	prevTotals, err := d.Totals(ctx, prev)
	if err != nil {
		return Compare{}, err
	}
	return Compare{Previous: prevTotals, PreviousFrom: prev.From, PreviousTo: prev.To}, nil
}
