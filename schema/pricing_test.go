package schema

import (
	"math"
	"reflect"
	"testing"
)

func TestGLMNotPricedAsAnthropic(t *testing.T) {
	pt := DefaultPriceTable()
	glm, ok := pt.Lookup("zai", "glm-5.3")
	if !ok {
		t.Fatal("glm-5.3 at zai should be priced")
	}
	opus, _ := pt.Lookup("", "claude-opus-5")
	if glm.Input >= opus.Input {
		t.Fatalf("glm input %.2f should be well below opus %.2f", glm.Input, opus.Input)
	}
}

// Rates must match Anthropic's published price list.
func TestOfficialRates(t *testing.T) {
	pt := DefaultPriceTable()
	for _, c := range []struct {
		model                         string
		in, out, cacheRead, cw5, cw1h float64
	}{
		{"claude-opus-5", 5, 25, 0.50, 6.25, 10},
		{"claude-sonnet-5", 2, 10, 0.20, 2.50, 4},
		{"claude-haiku-4-5", 1, 5, 0.10, 1.25, 2},
		// Fable 5.1 reads cache at 0.025x base rather than the usual 0.1x.
		{"claude-fable-5-1", 10, 50, 0.25, 12.50, 20},
		// Opus 5.5 reads cache at 0.05x.
		{"claude-opus-5-5", 4, 20, 0.20, 5, 8},
		// Upstream omits this alias's 1h price; the fallback must give the
		// published 2x input, not 2x the 5m write.
		{"claude-4-sonnet-20250514", 3, 15, 0.30, 3.75, 6},
	} {
		r, ok := pt.Lookup("", c.model)
		if !ok {
			t.Errorf("%s not priced", c.model)
			continue
		}
		if r.Input != c.in || r.Output != c.out || r.CacheRead != c.cacheRead ||
			r.CacheWrite5m != c.cw5 || r.CacheWrite1h != c.cw1h {
			t.Errorf("%s = %+v, want in=%v out=%v cr=%v cw5=%v cw1h=%v",
				c.model, r, c.in, c.out, c.cacheRead, c.cw5, c.cw1h)
		}
	}
}

func TestInferenceGeoPremium(t *testing.T) {
	pt := DefaultPriceTable()
	base := &Event{Model: "claude-opus-5", Usage: Usage{InputTokens: 1_000_000}}
	us := &Event{Model: "claude-opus-5", InferenceGeo: "us", Usage: Usage{InputTokens: 1_000_000}}
	b, _ := pt.Cost(base)
	u, _ := pt.Cost(us)
	if diff := u - b*1.1; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("us inference = %v, want %v (1.1x of %v)", u, b*1.1, b)
	}
}

func TestFastModeDoublesOpusOnly(t *testing.T) {
	pt := DefaultPriceTable()
	opus, _ := pt.Cost(&Event{Model: "claude-opus-5", Usage: Usage{InputTokens: 1_000_000}})
	opusFast, _ := pt.Cost(&Event{Model: "claude-opus-5", Speed: "fast", Usage: Usage{InputTokens: 1_000_000}})
	if opusFast != opus*2 {
		t.Fatalf("fast opus = %v, want %v", opusFast, opus*2)
	}

	son, _ := pt.Cost(&Event{Model: "claude-sonnet-5", Usage: Usage{InputTokens: 1_000_000}})
	sonFast, _ := pt.Cost(&Event{Model: "claude-sonnet-5", Speed: "fast", Usage: Usage{InputTokens: 1_000_000}})
	if sonFast != son {
		t.Fatalf("fast mode must not apply to sonnet: %v vs %v", sonFast, son)
	}
}

func TestWebSearchBilledPerCall(t *testing.T) {
	pt := DefaultPriceTable()
	usd, priced := pt.Cost(&Event{Model: "claude-opus-5", Usage: Usage{WebSearchCalls: 1000}})
	if !priced {
		t.Fatal("should be priced")
	}
	if usd != 10 {
		t.Fatalf("1000 searches = $%v, want $10", usd)
	}
}

// Dated model variants should resolve without enumerating every release.
func TestPrefixMatch(t *testing.T) {
	pt := DefaultPriceTable()
	r, ok := pt.Lookup("", "claude-opus-5-20260101")
	if !ok {
		t.Fatal("dated opus variant should resolve by prefix")
	}
	if r.Input != 5 {
		t.Fatalf("got input %v, want 5 (Opus 5 official rate)", r.Input)
	}
}

func TestUnknownModelIsUnpricedNotFree(t *testing.T) {
	pt := DefaultPriceTable()
	e := &Event{Model: "some-model-we-have-never-seen", Usage: Usage{InputTokens: 1_000_000}}
	usd, priced := pt.Cost(e)
	if priced {
		t.Fatal("unknown model should not be priced")
	}
	if usd != 0 {
		t.Fatalf("unpriced cost should be 0, got %v", usd)
	}
}

func TestCacheTTLsPricedDifferently(t *testing.T) {
	pt := DefaultPriceTable()
	short := &Event{Model: "claude-opus-5", Usage: Usage{CacheWrite5mTokens: 1_000_000}}
	long := &Event{Model: "claude-opus-5", Usage: Usage{CacheWrite1hTokens: 1_000_000}}
	s, _ := pt.Cost(short)
	l, _ := pt.Cost(long)
	if l <= s {
		t.Fatalf("1h cache write (%.2f) should cost more than 5m (%.2f)", l, s)
	}
}

func TestReasoningNotDoubleCounted(t *testing.T) {
	u := Usage{OutputTokens: 100, ReasoningTokens: 80}
	if got := u.TotalTokens(); got != 100 {
		t.Fatalf("got %d, want 100 (reasoning is a subset of output)", got)
	}
}

func TestMakeIDStableAndScoped(t *testing.T) {
	a := MakeID(SourceClaudeCode, "req_123")
	if a != MakeID(SourceClaudeCode, "req_123") {
		t.Fatal("ID must be stable")
	}
	if a == MakeID(SourceCodex, "req_123") {
		t.Fatal("same native id in different sources must not collide")
	}
}

func TestWebSearchFeeIsNotMultiplied(t *testing.T) {
	pt := DefaultPriceTable()
	plain, _ := pt.Cost(&Event{Model: "claude-opus-5", Usage: Usage{WebSearchCalls: 1000}})
	loaded, _ := pt.Cost(&Event{
		Model: "claude-opus-5", Speed: "fast", InferenceGeo: "us",
		Usage: Usage{WebSearchCalls: 1000},
	})
	if plain != 10 || loaded != 10 {
		t.Fatalf("1000 searches cost $%v plain and $%v with multipliers, want $10 both", plain, loaded)
	}
}

// Anthropic's caching multipliers apply on top of fast-mode pricing, so a cache
// read on fast Opus 5 is 0.1 x $10, not 0.1 x $5.
func TestFastModeDoublesEveryTokenCategory(t *testing.T) {
	pt := DefaultPriceTable()
	for _, c := range []struct {
		name string
		geo  string
		u    Usage
		want float64
	}{
		{"input", "", Usage{InputTokens: 1_000_000}, 10},
		{"output", "", Usage{OutputTokens: 1_000_000}, 50},
		{"cache read", "", Usage{CacheReadTokens: 1_000_000}, 1},
		{"5m cache write", "", Usage{CacheWrite5mTokens: 1_000_000}, 12.5},
		{"1h cache write", "", Usage{CacheWrite1hTokens: 1_000_000}, 20},
		{"US-pinned cache read", "us", Usage{CacheReadTokens: 1_000_000}, 1.1},
		{"a typical turn", "", Usage{InputTokens: 50, OutputTokens: 500,
			CacheReadTokens: 150_000, CacheWrite5mTokens: 3_000},
			2 * (50*5 + 500*25 + 150_000*0.5 + 3_000*6.25) / 1e6},
	} {
		got, _ := pt.Cost(&Event{Model: "claude-opus-5", Speed: "fast", InferenceGeo: c.geo, Usage: c.u})
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s on fast Opus 5 = $%.6f, want $%.6f", c.name, got, c.want)
		}
	}
}

// A character-prefix match would bill each of these at the rate of the listed
// model beside it, with nothing to say the number is a guess.
func TestARealisticUnknownModelIsUnpricedNotMispriced(t *testing.T) {
	pt := DefaultPriceTable()
	for _, model := range []string{
		"claude-opus-4-9", // claude-opus-4, 3x over
		"gpt-5.7",         // gpt-5, 70% under
		"gpt-5-codex-max", // gpt-5-codex
		"glm-5.9",         // glm-5
		"kimi-k3-turbo",   // kimi-k3
		"grok-4-fast-9",   // grok-4
		"o3-ultra",        // o3
	} {
		if _, ok := pt.Lookup("", model); ok {
			t.Errorf("%q resolved to a rate; an unknown model must be unpriced", model)
		}
	}
}

func TestDatedVariantsStillResolve(t *testing.T) {
	pt := DefaultPriceTable()
	base, ok := pt.Lookup("", "claude-opus-5")
	if !ok {
		t.Fatal("claude-opus-5 is not in the table")
	}
	for _, model := range []string{
		"claude-opus-5-20260101",
		"claude-opus-5@20260101",
	} {
		r, ok := pt.Lookup("", model)
		if !ok {
			t.Errorf("%q did not resolve", model)
			continue
		}
		if r.Input != base.Input || r.Output != base.Output {
			t.Errorf("%q resolved to %v/%v, want the base rate %v/%v",
				model, r.Input, r.Output, base.Input, base.Output)
		}
	}
}

func TestDatedVariant(t *testing.T) {
	for _, s := range []string{"", "-20260101", "@20260101", ":202601", "-v20260101"} {
		if !datedVariant(s) {
			t.Errorf("datedVariant(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"-9", "-5", ".7", "-codex-max", "-turbo", "-fast-9", "-ultra", "x",
		"-latest", "-default", "-2026-09-01",
	} {
		if datedVariant(s) {
			t.Errorf("datedVariant(%q) = true, want false", s)
		}
	}
}

// A base and its -latest alias can carry different rates upstream, so an
// unlisted alias priced as its base is a confidently wrong number.
func TestAnUnlistedLatestAliasIsUnpricedNotGuessed(t *testing.T) {
	pt := DefaultPriceTable()
	if _, ok := pt.Lookup("", "claude-opus-5"); !ok {
		t.Fatal("claude-opus-5 is not in the table")
	}
	for _, model := range []string{
		"claude-opus-5-latest",
		"claude-opus-5-default",
		"claude-3-7-sonnet-latest",
	} {
		if r, ok := pt.Lookup("", model); ok {
			t.Errorf("%q resolved to %v/%v -- an alias absent from the table "+
				"must be reported unpriced, not given its base's rate",
				model, r.Input, r.Output)
		}
	}
}

// gpt-5.6-sol is 4/20, cache read 0.4, and 8/30/0.8 above 272k tokens. Opus 5
// is given a hypothetical 7.5/37.5 tier to show that fast mode's flat price
// replaces a tier rather than doubling it.
func TestLongContextTiersPriceTheWholeRequest(t *testing.T) {
	pt := DefaultPriceTable()
	opus := pt.Rates["|claude-opus-5"]
	opus.Tiers = []Tier{{Above: 200_000, Prices: Prices{
		Input: 7.5, Output: 37.5, CacheRead: 0.75, CacheWrite5m: 9.375, CacheWrite1h: 15}}}
	pt.Rates["|claude-opus-5"] = opus
	past := Usage{InputTokens: 12_001, CacheReadTokens: 260_000, OutputTokens: 10_000}
	at := Usage{InputTokens: 12_000, CacheReadTokens: 260_000, OutputTokens: 10_000}
	for _, c := range []struct {
		name string
		e    Event
		want float64
	}{
		{"a prompt at the threshold stays at base",
			Event{Source: SourceCodex, Model: "gpt-5.6-sol", Usage: at},
			(12_000*4 + 260_000*0.4 + 10_000*20) / 1e6},
		{"one token past it pays the tier throughout",
			Event{Source: SourceCodex, Model: "gpt-5.6-sol", Usage: past},
			(12_001*8 + 260_000*0.8 + 10_000*30) / 1e6},
		{"cache writes count toward the prompt",
			Event{Source: SourceClaudeCode, Model: "claude-sonnet-4-5",
				Usage: Usage{InputTokens: 1_000, CacheWrite5mTokens: 150_000, CacheWrite1hTokens: 49_001}},
			(1_000*6 + 150_000*7.5 + 49_001*12) / 1e6},
		{"a Gemini session total is not one request",
			Event{Source: SourceGemini, Model: "gpt-5.6-sol", Usage: past},
			(12_001*4 + 260_000*0.4 + 10_000*20) / 1e6},
		{"a Copilot turn is not one request",
			Event{Source: SourceCopilot, Model: "gpt-5.6-sol", Usage: past},
			(12_001*4 + 260_000*0.4 + 10_000*20) / 1e6},
		{"a Kimi turn is not one request",
			Event{Source: SourceKimi, Model: "gpt-5.6-sol", Usage: past},
			(12_001*4 + 260_000*0.4 + 10_000*20) / 1e6},
		{"an event of unknown source is not shown to be one request",
			Event{Model: "gpt-5.6-sol", Usage: past},
			(12_001*4 + 260_000*0.4 + 10_000*20) / 1e6},
		{"standard-speed Opus pays its tier",
			Event{Source: SourceClaudeCode, Model: "claude-opus-5", Usage: Usage{InputTokens: 300_000}},
			300_000 * 7.5 / 1e6},
		{"fast Opus pays its flat fast price instead",
			Event{Source: SourceClaudeCode, Model: "claude-opus-5", Speed: "fast", Usage: Usage{InputTokens: 300_000}},
			300_000 * 5 * 2 / 1e6},
	} {
		got, priced := pt.Cost(&c.e)
		if !priced || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: $%.6f (priced=%v), want $%.6f", c.name, got, priced, c.want)
		}
	}
}

// An endpoint the table does not know falls back to the bare key, so a
// reseller's price there would bill everyone at it.
func TestABareModelKeyIsItsMakersPrice(t *testing.T) {
	pt := DefaultPriceTable()
	for _, c := range []struct{ maker, model string }{
		{"xai", "grok-4"},
		{"deepseek", "deepseek-r1"},
		{"zai", "glm-5.1"},
		{"moonshot", "kimi-k3"},
	} {
		bare, ok := pt.Lookup("", c.model)
		own, ownOK := pt.Lookup(c.maker, c.model)
		if !ok || !ownOK {
			t.Errorf("%s: bare priced=%v, %s priced=%v", c.model, ok, c.maker, ownOK)
			continue
		}
		if !reflect.DeepEqual(bare, own) {
			t.Errorf("%s with no endpoint = %+v, but %s's own price is %+v",
				c.model, bare, c.maker, own)
		}
	}
	// Only resellers sell this one, so with no endpoint there is no price to take.
	if r, ok := pt.Lookup("", "gpt-oss-120b"); ok {
		t.Errorf("gpt-oss-120b with no endpoint priced at a reseller's %v/%v", r.Input, r.Output)
	}
}

func TestALocalRuntimeIsFreeNotBilledAtACloudRate(t *testing.T) {
	pt := DefaultPriceTable()
	u := Usage{InputTokens: 50_000_000, OutputTokens: 5_000_000}
	for _, c := range []struct{ endpoint, model string }{
		{"lmstudio", "openai/gpt-oss-20b"},
		{"ollama", "gpt-oss-120b"},
		{"LMStudio", "qwen/qwen3-coder-30b"},
		{"llama.cpp", "deepseek-ai/DeepSeek-V3"},
		{"vllm", "Qwen/Qwen3-Coder-30B-A3B-Instruct"},
	} {
		usd, priced := pt.Cost(&Event{Endpoint: c.endpoint, Model: c.model, CostBasis: CostBilled, Usage: u})
		if !priced || usd != 0 {
			t.Errorf("%s|%s = $%.2f (priced=%v), want $0 priced", c.endpoint, c.model, usd, priced)
		}
	}
	// A cloud endpoint the table does not list still takes the model's price.
	cloud, ok := pt.Lookup("requesty", "claude-opus-5")
	own, _ := pt.Lookup("", "claude-opus-5")
	if !ok || !reflect.DeepEqual(cloud, own) {
		t.Errorf("requesty|claude-opus-5 = %v/%v (ok=%v), want claude-opus-5's %v/%v",
			cloud.Input, cloud.Output, ok, own.Input, own.Output)
	}
}

// The local-runtime zero exists for exactly this case: a model served on the
// laptop under a name the table prices for its maker's cloud. Checked after
// the bare model, the runtime would never be reached for such a name, and
// TestALocalRuntimeIsFreeNotBilledAtACloudRate uses unpriced names only.
func TestALocalModelNamedLikeACloudOneIsStillFree(t *testing.T) {
	pt := DefaultPriceTable()
	for _, model := range []string{"deepseek-r1", "glm-5.1", "kimi-k3"} {
		if _, ok := pt.Lookup("", model); !ok {
			t.Fatalf("setup: %s is not priced for its maker's cloud", model)
		}
		e := &Event{Endpoint: "ollama", Model: model, Usage: Usage{InputTokens: 1_000_000}}
		if usd, priced := pt.Cost(e); !priced || usd != 0 {
			t.Errorf("ollama|%s = $%.2f (priced=%v), want $0: it ran on the laptop", model, usd, priced)
		}
	}
}
