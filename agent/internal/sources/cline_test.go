package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// clineCtx builds a collector pointed at a fake home containing one Cline task.
// Parsing tests call readTask, not Collect: Collect's commit empties the emit
// buffer before it returns.
func clineCtx(t *testing.T, messages string) (*Ctx, clineFamily) {
	t.Helper()
	home := t.TempDir()
	a := clineFamily{
		name: schema.SourceCline,
		root: "Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/tasks",
		alt:  "Library/Application Support/Cursor/User/globalStorage/saoudrizwan.claude-dev/tasks",
	}
	dir := filepath.Join(home, a.root, "task-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui_messages.json"), []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Ctx{Store: st, MachineID: "m", Home: home}, a
}

func taskFile(c *Ctx, a clineFamily) string {
	return filepath.Join(c.Home, a.root, "task-1", "ui_messages.json")
}

func TestClineCollectStoresWhatItReads(t *testing.T) {
	c, a := clineCtx(t, `[
	  {"type":"say","say":"text","ts":1790000000000,
	   "modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5","mode":"act"}},
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":3638,\"tokensOut\":409,\"cost\":0.01}"}
	]`)
	res, err := a.Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 1 {
		t.Fatalf("stored %d rows, want 1", res.Stored)
	}

	// The watermark has moved past the task, so a second pass reads nothing.
	res2, err := a.Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stored != 0 {
		t.Fatalf("a second pass re-read %d rows, want 0", res2.Stored)
	}
}

func TestClineDecodesTheNestedUsageBlob(t *testing.T) {
	c, a := clineCtx(t, `[
	  {"type":"say","say":"text","ts":1790000000000,
	   "modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5","mode":"act"}},
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":3638,\"tokensOut\":409,\"cacheWrites\":12,\"cacheReads\":900,\"cost\":0.011366}"}
	]`)

	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Usage.InputTokens != 3638 || e.Usage.OutputTokens != 409 {
		t.Fatalf("tokens in/out = %d/%d, want 3638/409", e.Usage.InputTokens, e.Usage.OutputTokens)
	}
	if e.Usage.CacheReadTokens != 900 || e.Usage.CacheWrite5mTokens != 12 {
		t.Fatalf("cache read/write = %d/%d, want 900/12",
			e.Usage.CacheReadTokens, e.Usage.CacheWrite5mTokens)
	}
	if e.NativeCostUSD == nil || *e.NativeCostUSD != 0.011366 {
		t.Fatalf("cost = %v, want 0.011366", e.NativeCostUSD)
	}
}

// Reading only the older apiProtocol field prices every task as unknown.
func TestClineTakesModelFromModelInfo(t *testing.T) {
	c, a := clineCtx(t, `[
	  {"type":"say","say":"text","ts":1790000000000,
	   "modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5","mode":"act"}},
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":10,\"tokensOut\":5,\"cost\":0.1}"}
	]`)
	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Model != "claude-opus-5" || events[0].Provider != "anthropic" {
		t.Fatalf("got %s/%s, want anthropic/claude-opus-5", events[0].Provider, events[0].Model)
	}
}

func TestClineKeepsRequestsInTheSameMillisecondApart(t *testing.T) {
	c, a := clineCtx(t, `[
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":10,\"tokensOut\":5}"},
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":20,\"tokensOut\":7}"}
	]`)
	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].ID == events[1].ID {
		t.Fatal("both requests share an id, so one would overwrite the other")
	}
}

func TestClineSkipsAHalfWrittenTask(t *testing.T) {
	c, a := clineCtx(t, `[{"type":"say","say":"api_req_started","ts":179000000`)
	res, err := a.Collect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 0 {
		t.Fatalf("stored %d rows from a truncated file, want 0", res.Stored)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("a task still being written is not an error: %v", res.Errors)
	}
}

func TestClineIgnoresNonRequestEntries(t *testing.T) {
	c, a := clineCtx(t, `[
	  {"type":"say","say":"text","ts":1790000000000},
	  {"type":"ask","say":"tool","ts":1790000000500},
	  {"type":"say","say":"api_req_started","ts":1790000001000,
	   "text":"{\"tokensIn\":0,\"tokensOut\":0}"}
	]`)
	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0 -- a zero-token request is not usage", len(events))
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

func TestClineKeySurvivesAMessageBeingDeleted(t *testing.T) {
	req := `{"type":"say","say":"api_req_started","ts":1758000000000,` +
		`"modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5"},` +
		`"text":"{\"tokensIn\":1000,\"tokensOut\":200,\"cost\":0.5}"}`
	chatter := `{"type":"say","say":"text","ts":1757999999000,"text":"hello"}`

	before, a := clineCtx(t, "["+chatter+","+req+"]")
	if _, err := a.readTask(before, taskFile(before, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	evBefore, _ := before.Drain()

	// The same request, with the unrelated message above it removed.
	after, a2 := clineCtx(t, "["+req+"]")
	if _, err := a2.readTask(after, taskFile(after, a2), "task-1"); err != nil {
		t.Fatal(err)
	}
	evAfter, _ := after.Drain()

	if len(evBefore) != 1 || len(evAfter) != 1 {
		t.Fatalf("got %d then %d events, want 1 each", len(evBefore), len(evAfter))
	}
	if evBefore[0].ID != evAfter[0].ID {
		t.Fatalf("the same request changed identity when an unrelated message was "+
			"deleted: %s -> %s (native %q -> %q); it would be counted twice",
			evBefore[0].ID, evAfter[0].ID, evBefore[0].NativeID, evAfter[0].NativeID)
	}
}

func TestClineKeepsSameMillisecondRequestsDistinct(t *testing.T) {
	one := `{"type":"say","say":"api_req_started","ts":1758000000000,` +
		`"modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5"},` +
		`"text":"{\"tokensIn\":1000,\"tokensOut\":200,\"cost\":0.5}"}`
	two := `{"type":"say","say":"api_req_started","ts":1758000000000,` +
		`"modelInfo":{"providerId":"anthropic","modelId":"claude-opus-5"},` +
		`"text":"{\"tokensIn\":7,\"tokensOut\":9,\"cost\":0.01}"}`

	c, a := clineCtx(t, "["+one+","+two+"]")
	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].ID == events[1].ID {
		t.Fatal("two requests in the same millisecond share an id; one would be lost")
	}
}

func TestClineOmitsNativeCostWhenClineReportedNone(t *testing.T) {
	noCost := `{"type":"say","say":"api_req_started","ts":1758000000000,` +
		`"modelInfo":{"providerId":"ollama","modelId":"qwen3-coder:30b"},` +
		`"text":"{\"tokensIn\":12000000,\"tokensOut\":3000000}"}`

	c, a := clineCtx(t, "["+noCost+"]")
	if _, err := a.readTask(c, taskFile(c, a), "task-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Drain()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].NativeCostUSD != nil {
		t.Fatalf("an absent cost became an authoritative %v; it must fall through "+
			"to the price table", *events[0].NativeCostUSD)
	}

	// A reported zero is still authoritative: a local model really is free.
	withZero := `{"type":"say","say":"api_req_started","ts":1758000000000,` +
		`"modelInfo":{"providerId":"ollama","modelId":"qwen3-coder:30b"},` +
		`"text":"{\"tokensIn\":100,\"tokensOut\":20,\"cost\":0}"}`
	c2, a2 := clineCtx(t, "["+withZero+"]")
	if _, err := a2.readTask(c2, taskFile(c2, a2), "task-1"); err != nil {
		t.Fatal(err)
	}
	ev2, _ := c2.Drain()
	if len(ev2) != 1 || ev2[0].NativeCostUSD == nil || *ev2[0].NativeCostUSD != 0 {
		t.Fatal("a reported zero cost must be preserved as zero")
	}
}
