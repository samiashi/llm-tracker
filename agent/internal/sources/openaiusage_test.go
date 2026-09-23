package sources

import (
	"testing"
)

func TestDeepSeekCacheHitsNotDoubleCounted(t *testing.T) {
	u := openAIUsage{PromptTokens: 1000, PromptCacheHitTokens: 700, PromptCacheMissTokens: 300, CompletionTokens: 50}
	got := u.normalise()
	if got.InputTokens != 300 || got.CacheReadTokens != 700 || got.OutputTokens != 50 {
		t.Fatalf("got in=%d cacheRead=%d out=%d, want 300/700/50",
			got.InputTokens, got.CacheReadTokens, got.OutputTokens)
	}
}

// The OpenAI details form must reach the same answer as the DeepSeek form.
func TestCachedTokensDetailsForm(t *testing.T) {
	u := openAIUsage{PromptTokens: 1000, CompletionTokens: 50}
	u.PromptTokensDetails = &struct {
		CachedTokens int64 `json:"cached_tokens"`
	}{CachedTokens: 700}
	got := u.normalise()
	if got.InputTokens != 300 || got.CacheReadTokens != 700 {
		t.Fatalf("got in=%d cacheRead=%d, want 300/700", got.InputTokens, got.CacheReadTokens)
	}
}
