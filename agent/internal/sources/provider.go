package sources

import "strings"

// inferProvider guesses the provider from the model name.
//
// Claude Code records no base URL, so a model reached through
// ANTHROPIC_BASE_URL is indistinguishable from a native one except by name.
// Anything unrecognised stays "unknown" so it shows up as unpriced rather than
// being quietly charged at Anthropic rates.
func inferProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "glm"):
		return "zai"
	case strings.HasPrefix(m, "kimi"):
		return "moonshotai"
	case strings.HasPrefix(m, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return "openai"
	case m == "":
		return ""
	default:
		return "unknown"
	}
}

// modelProvider guesses a provider from a model name for the records where
// Continue omits it. Kept deliberately small: a wrong guess prices an event
// against the wrong table, so anything unrecognised stays empty.
func modelProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return "openai"
	case strings.HasPrefix(m, "gemini"):
		return "gemini"
	}
	return ""
}
