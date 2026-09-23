package sources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
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

func TestCacheTTLsKeptApart(t *testing.T) {
	line := []byte(`{"type":"assistant","requestId":"req_1","timestamp":"2026-09-22T10:00:00Z",
	  "message":{"id":"msg_1","model":"claude-opus-5","usage":{
	    "input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,
	    "cache_creation_input_tokens":99,
	    "cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":50}}}}`)
	var l anthropicLine
	if err := json.Unmarshal(line, &l); err != nil {
		t.Fatal(err)
	}
	ev, ok := l.toEvent(schema.SourceClaudeCode, &Ctx{MachineID: "m"}, "acct")
	if !ok {
		t.Fatal("expected a usage event")
	}
	if ev.Usage.CacheWrite5mTokens != 40 || ev.Usage.CacheWrite1hTokens != 50 {
		t.Fatalf("got 5m=%d 1h=%d, want 40/50 -- the TTLs are billed differently",
			ev.Usage.CacheWrite5mTokens, ev.Usage.CacheWrite1hTokens)
	}
}

func TestCombinedCacheFallsBackToCheaperBucket(t *testing.T) {
	line := []byte(`{"type":"assistant","requestId":"r","timestamp":"2026-09-22T10:00:00Z",
	  "message":{"id":"m","model":"claude-opus-5","usage":{
	    "input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":77}}}`)
	var l anthropicLine
	json.Unmarshal(line, &l)
	ev, ok := l.toEvent(schema.SourceClaudeCode, &Ctx{}, "a")
	if !ok {
		t.Fatal("expected event")
	}
	if ev.Usage.CacheWrite5mTokens != 77 || ev.Usage.CacheWrite1hTokens != 0 {
		t.Fatalf("got 5m=%d 1h=%d, want the combined figure in the cheaper bucket",
			ev.Usage.CacheWrite5mTokens, ev.Usage.CacheWrite1hTokens)
	}
}

func TestNonUsageLinesProduceNothing(t *testing.T) {
	for _, raw := range []string{
		`{"type":"user","message":{"id":"m"}}`,
		`{"type":"assistant","requestId":"r","message":{"id":"m","model":"x","usage":{}}}`,
	} {
		var l anthropicLine
		json.Unmarshal([]byte(raw), &l)
		if _, ok := l.toEvent(schema.SourceClaudeCode, &Ctx{}, "a"); ok {
			t.Fatalf("%s should not produce an event", raw)
		}
	}
}

func TestBothKeyCasingsAccepted(t *testing.T) {
	var camel, snake anthropicLine
	json.Unmarshal([]byte(`{"requestId":"a","sessionId":"s1"}`), &camel)
	json.Unmarshal([]byte(`{"request_id":"a","session_id":"s1"}`), &snake)
	if camel.requestID() != snake.requestID() || camel.sessionID() != snake.sessionID() {
		t.Fatal("camelCase and snake_case spellings must resolve identically")
	}
}

func TestProviderInferredFromModelName(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5":  "anthropic",
		"glm-5.3":        "zai",
		"kimi-k3":        "moonshotai",
		"deepseek-flash": "deepseek",
		"gpt-6-astra":    "openai",
		"something-new":  "unknown",
	} {
		if got := inferProvider(model); got != want {
			t.Errorf("inferProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

// Cowork files each session under the account that ran it, which is exact
// even for history read long after a switch of login; whoever is signed in
// now is only the fallback.
func TestCoworkCreditsTheAccountItsSessionIsFiledUnder(t *testing.T) {
	const owner = "0a1b2c3d-0000-4000-8000-00000000abcd"
	home := t.TempDir()
	dir := filepath.Join(home, "Library/Application Support/Claude/local-agent-mode-sessions",
		owner, "0a1b2c3d-0000-4000-8000-0000000000aa", "0a1b2c3d-0000-4000-8000-0000000000bb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","request_id":"req_1","session_id":"s1","timestamp":"2026-09-22T10:00:00Z",` +
		`"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &Ctx{Store: st, MachineID: "m", Home: home, Accounts: map[string]*schema.Account{
		"anthropic": {Ref: "anthropic:someone-signed-in-today", Provider: "anthropic"},
	}}
	if _, err := (Cowork{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	evs := storedEvents(t, st)
	if len(evs) != 1 || evs[0].AccountRef != "anthropic:"+owner ||
		evs[0].Surface != schema.SurfaceDesktop || evs[0].CostBasis != schema.CostRateCard {
		t.Fatalf("events %+v, want one desktop event credited to %s at rate card", evs, owner)
	}
}
