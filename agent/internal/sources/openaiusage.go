package sources

import "github.com/samiashi/llm-tracker/schema"

// openAIUsage is the usage object every OpenAI-compatible provider speaks,
// DeepSeek, Z.ai and Moonshot included.
//
// Harnesses wrapping the same API spell its fields differently, so the common
// spellings are all declared and whichever is populated wins.
type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`

	// DeepSeek splits prompt tokens into cache hits and misses rather than
	// reporting a separate cache-read counter.
	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`

	// Some wrappers pass through the OpenAI details objects instead.
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	// Anthropic-style spellings, seen in harnesses that started as Claude Code
	// forks.
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// normalise folds the spellings into the common shape.
//
// Cached tokens are subtracted from the prompt total wherever the provider
// reports them as a component of it, so the cached portion is never charged at
// both the full and the cache rate.
func (u openAIUsage) normalise() schema.Usage {
	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}

	var cacheRead int64
	switch {
	case u.PromptCacheHitTokens > 0 || u.PromptCacheMissTokens > 0:
		cacheRead = u.PromptCacheHitTokens
		if u.PromptCacheMissTokens > 0 {
			in = u.PromptCacheMissTokens
		} else {
			in = max(in-cacheRead, 0)
		}
	case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0:
		cacheRead = u.PromptTokensDetails.CachedTokens
		in = max(in-cacheRead, 0)
	case u.CacheReadTokens > 0:
		cacheRead = u.CacheReadTokens
	}

	usage := schema.Usage{
		InputTokens: in, OutputTokens: out,
		CacheReadTokens: cacheRead, CacheWrite5mTokens: u.CacheCreationTokens,
	}
	if u.CompletionTokensDetails != nil {
		usage.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return usage
}

func (u openAIUsage) empty() bool { return u.normalise().TotalTokens() == 0 }
