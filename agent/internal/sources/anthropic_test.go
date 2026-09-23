package sources

import (
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

// The line is written twice, as streaming repeats it; the attempt counts once.
func TestClaudeCodeCountsTheAttemptAFallbackAbandoned(t *testing.T) {
	line := `{"type":"assistant","requestId":"req_1","sessionId":"s1","timestamp":"2026-09-22T10:00:00Z",` +
		`"message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":20,` +
		`"iterations":[` +
		`{"type":"message","model":"claude-opus-5","input_tokens":1000,"output_tokens":50,"cache_read_input_tokens":300},` +
		`{"type":"fallback_message","model":"claude-sonnet-5","input_tokens":10,"output_tokens":20}]}}}`
	events := collectFile(t, ClaudeCode{}, ".claude/projects/-Users-dev-app/s1.jsonl", line+"\n"+line+"\n")

	byModel := map[string]schema.Event{}
	for _, e := range events {
		byModel[e.Model] = e
	}
	if len(events) != 2 {
		t.Fatalf("stored %d events, want the response and its abandoned attempt", len(events))
	}
	if u := byModel["claude-sonnet-5"].Usage; u.InputTokens != 10 || u.OutputTokens != 20 {
		t.Errorf("response usage = %+v, want the top-level figures", u)
	}
	a := byModel["claude-opus-5"]
	if a.Usage.InputTokens != 1000 || a.Usage.OutputTokens != 50 || a.Usage.CacheReadTokens != 300 {
		t.Errorf("abandoned attempt usage = %+v, want its own figures", a.Usage)
	}
	if a.Provider != "anthropic" || a.SessionID != "s1" {
		t.Errorf("abandoned attempt provider=%q session=%q, want anthropic / s1", a.Provider, a.SessionID)
	}
}

func TestASingleIterationAddsNoEvent(t *testing.T) {
	line := `{"type":"assistant","requestId":"req_1","timestamp":"2026-09-22T10:00:00Z",` +
		`"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20,` +
		`"iterations":[{"type":"message","input_tokens":10,"output_tokens":20}]}}}`
	if events := collectFile(t, ClaudeCode{}, ".claude/projects/-Users-dev-app/s1.jsonl", line+"\n"); len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
}
