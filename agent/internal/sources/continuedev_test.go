package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
)

// Continue's timestamps have second precision, and undated records share the
// file's mtime, so a key built on time would collapse them into one.
func TestContinueKeepsRecordsInTheSameSecondDistinct(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".continue/dev_data/0.2.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same second, same model, different sizes -- and two undated records.
	body := `{"timestamp":"2026-09-20T10:00:00Z","model":"claude-opus-5","promptTokens":1000,"generatedTokens":200}
{"timestamp":"2026-09-20T10:00:00Z","model":"claude-opus-5","promptTokens":7,"generatedTokens":9}
{"model":"claude-opus-5","promptTokens":50,"generatedTokens":5}
{"model":"claude-opus-5","promptTokens":60,"generatedTokens":6}
`
	if err := os.WriteFile(filepath.Join(dir, "tokensGenerated.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	c := &Ctx{Store: st, MachineID: "m", Home: home}
	if _, err := (ContinueDev{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ids, _, err := st.Unsent(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 {
		t.Fatalf("stored %d events, want 4 -- records sharing a timestamp must "+
			"stay distinct", len(ids))
	}
}

// Reading only one shape silently drops every record written in the other.
func TestContinueReadsBothRecordShapes(t *testing.T) {
	for name, line := range map[string]string{
		"flat":   `{"promptTokens":100,"generatedTokens":20,"model":"claude-opus-5","provider":"anthropic","timestamp":"2026-09-20T10:00:00Z"}`,
		"nested": `{"eventName":"tokensGenerated","timestamp":"2026-09-20T10:00:00Z","data":{"promptTokens":100,"generatedTokens":20,"model":"claude-opus-5","provider":"anthropic"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			events := collectFile(t, ContinueDev{}, ".continue/dev_data/0.2.0/tokensGenerated.jsonl", line+"\n")
			if len(events) != 1 {
				t.Fatalf("stored %d events, want 1", len(events))
			}
			if u := events[0].Usage; u.InputTokens != 100 || u.OutputTokens != 20 {
				t.Fatalf("got %d/%d, want 100/20", u.InputTokens, u.OutputTokens)
			}
		})
	}
}

func TestContinueProviderGuessStaysConservative(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5":    "anthropic",
		"gpt-6-astra":      "openai",
		"gemini-3-pro":     "google",
		"llama-3-70b":      "",
		"some-local-thing": "",
	} {
		if got := modelProvider(model); got != want {
			t.Errorf("modelProvider(%q) = %q, want %q", model, got, want)
		}
	}
}
