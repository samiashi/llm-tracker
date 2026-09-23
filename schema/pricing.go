package schema

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strings"
	"sync"
)

// pricesJSON is generated from LiteLLM's price database by `make prices`
// (schema/scripts/gen_prices.py). Edit the generator, never the file
// (AGENTS.md invariant 8).
//
//go:embed prices.json
var pricesJSON []byte

// Prices are USD per 1M tokens.
type Prices struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
}

// Rate is what one model costs at one endpoint.
type Rate struct {
	Prices

	// WebSearchPerCall is billed per invocation rather than per token. Web
	// fetch is free beyond the tokens it returns, so it has no matching field.
	WebSearchPerCall float64 `json:"web_search_per_call,omitempty"`

	// Tiers are long-context prices. A request whose prompt exceeds a tier's
	// Above is billed at that tier in every category, not only on the tokens
	// past the threshold.
	Tiers []Tier `json:"tiers,omitempty"`
}

// Tier is the prices a request pays once its prompt exceeds Above tokens.
type Tier struct {
	Above int64 `json:"above"`
	Prices
}

// PriceTable maps "endpoint|model" to a rate.
//
// Matching on model alone is not enough: Claude Code pointed at a GLM or Kimi
// endpoint writes the same log format, and applying Anthropic rates to those
// tokens is an order-of-magnitude error.
type PriceTable struct {
	Version string          `json:"version"`
	Source  string          `json:"source,omitempty"`
	Rates   map[string]Rate `json:"rates"`
}

// PriceKey builds the lookup key for a model at an endpoint.
func PriceKey(endpoint, model string) string {
	return strings.ToLower(endpoint) + "|" + strings.ToLower(model)
}

var (
	bundledOnce sync.Once
	bundled     *PriceTable
)

// DefaultPriceTable returns the generated table.
func DefaultPriceTable() *PriceTable {
	bundledOnce.Do(func() {
		bundled = &PriceTable{Rates: map[string]Rate{}}
		if err := json.Unmarshal(pricesJSON, bundled); err != nil {
			// A corrupt embed must not take the server down; an empty table
			// reports everything as unpriced, which is visible rather than wrong.
			bundled = &PriceTable{Version: "invalid", Rates: map[string]Rate{}}
		}
		for k, v := range bundled.Rates {
			if v.WebSearchPerCall == 0 {
				v.WebSearchPerCall = webSearchFee(k)
				bundled.Rates[k] = v
			}
		}
	})
	// Copied, tiers included, so a caller cannot mutate the shared table.
	r := make(map[string]Rate, len(bundled.Rates))
	for k, v := range bundled.Rates {
		v.Tiers = slices.Clone(v.Tiers)
		r[k] = v
	}
	return &PriceTable{Version: bundled.Version, Source: bundled.Source, Rates: r}
}

// webSearchFee is the per-call price LiteLLM does not carry: Anthropic's $10
// per 1,000 web searches. Without it server-tool spend on Claude disappears.
func webSearchFee(key string) float64 {
	if strings.Contains(key, "claude") {
		return 10.0 / 1000
	}
	return 0
}

// localRuntimes are endpoints that run the model on the developer's own
// hardware. Local inference has no bill, so it prices at zero rather than at
// whatever a cloud provider charges for the same model name.
var localRuntimes = map[string]bool{
	"ollama":         true,
	"lmstudio":       true,
	"llama.cpp":      true,
	"llamafile":      true,
	"vllm":           true,
	"text-gen-webui": true,
	"msty":           true,
	"lemonade":       true,
}

// Lookup resolves a rate for an endpoint+model pair.
//
// It tries the exact pair, then a local runtime's zero, then the model alone --
// its maker's price, see FIRST_PARTY in gen_prices.py -- and then the model as
// a dated variant of a listed one. ok is false when nothing matched, and
// callers must surface that rather than treating a zero rate as free.
func (pt *PriceTable) Lookup(endpoint, model string) (Rate, bool) {
	if r, ok := pt.Rates[PriceKey(endpoint, model)]; ok {
		return r, true
	}
	if localRuntimes[strings.ToLower(endpoint)] {
		return Rate{}, true
	}
	if r, ok := pt.Rates[PriceKey("", model)]; ok {
		return r, true
	}
	lm := strings.ToLower(model)
	var best string
	for k := range pt.Rates {
		name, found := strings.CutPrefix(k, "|")
		if !found || name == "" || !strings.HasPrefix(lm, name) {
			continue
		}
		if !datedVariant(lm[len(name):]) {
			continue
		}
		if len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		return pt.Rates[PriceKey("", best)], true
	}
	return Rate{}, false
}

// datedVariant reports whether suffix is only a build stamp: a date (20260101,
// 202601), optionally v-prefixed. Anything looser prices an unknown model at a
// neighbour's rate -- claude-opus-4-9 as claude-opus-4, at 3x -- and "-latest"
// and "-default" are separately priced aliases, not stamps.
func datedVariant(suffix string) bool {
	if suffix == "" {
		return true
	}
	switch suffix[0] {
	case '-', '@', ':':
	default:
		return false
	}
	rest := strings.TrimPrefix(suffix[1:], "v")
	if len(rest) != 8 && len(rest) != 6 {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Cost prices one event.
//
// priced is false when no rate matched. Those tokens are reported as an
// explicit "unpriced" figure: returning 0 would make an unrecognised model look
// like free usage, hiding exactly the models worth noticing.
func (pt *PriceTable) Cost(e *Event) (usd float64, priced bool) {
	r, ok := pt.Lookup(e.Endpoint, e.Model)
	if !ok {
		return 0, false
	}
	u := e.Usage
	fast := e.fastModeApplies()

	// Fast mode's price holds across the whole context window, so it replaces
	// a long-context tier rather than stacking on one.
	p := r.Prices
	if !fast && e.coversOneRequest() {
		p = r.forPrompt(u.promptTokens())
	}

	const perToken = 1_000_000.0
	usd = (float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheReadTokens)*p.CacheRead +
		float64(u.CacheWrite5mTokens)*p.CacheWrite5m +
		float64(u.CacheWrite1hTokens)*p.CacheWrite1h) / perToken

	// Fast mode (2x) and US-only inference (1.1x) multiply every token
	// category, cache reads and writes included.
	if fast {
		usd *= 2
	}
	if strings.EqualFold(e.InferenceGeo, "us") {
		usd *= 1.1
	}

	// Server-side tools are billed per call at a flat rate, outside the token
	// subtotal, so neither multiplier touches them.
	usd += float64(u.WebSearchCalls) * r.WebSearchPerCall

	return usd, true
}

// forPrompt returns the prices of the highest tier the prompt exceeds, else
// the base prices.
func (r Rate) forPrompt(prompt int64) Prices {
	p, above := r.Prices, int64(0)
	for _, t := range r.Tiers {
		if prompt > t.Above && t.Above >= above {
			p, above = t.Prices, t.Above
		}
	}
	return p
}

// promptTokens is everything one request read -- fresh input, cache reads and
// cache writes -- which is what a long-context threshold counts.
func (u Usage) promptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWrite5mTokens + u.CacheWrite1hTokens
}

// coversOneRequest reports whether the event's usage is a single API
// request's. A tier prices a request by its own prompt, and a total summed over
// several requests crosses thresholds that none of them did.
func (e *Event) coversOneRequest() bool {
	switch e.Source {
	case SourceGemini, // session_*.json holds one running total per session
		SourceKimi, SourceCopilot, // turn-level records can span several requests
		"": // no provenance, so nothing shows it is one request
		return false
	}
	return true
}

// fastModeApplies reports whether the fast-mode premium is charged. Only Opus
// has one, and Speed is the API's own report of how the request ran.
func (e *Event) fastModeApplies() bool {
	return strings.EqualFold(e.Speed, "fast") &&
		strings.HasPrefix(strings.ToLower(e.Model), "claude-opus-")
}
