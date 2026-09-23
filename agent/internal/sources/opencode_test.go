package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// openCodeCtx builds a collector pointed at a fake home holding an opencode
// database with the given assistant messages.
func openCodeCtx(t *testing.T, messages ...string) *Ctx {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE message (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
		data TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, m := range messages {
		if _, err := db.Exec(
			`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
			fmt.Sprintf("msg_%d", i), "ses_1", 1_780_000_000_000, 1_780_000_000_000, m); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Ctx{Store: st, MachineID: "m", Home: home}
}

// collected runs the adapter and reads back what it stored.
func collected(t *testing.T, c *Ctx) []schema.Event {
	t.Helper()
	if _, err := (OpenCode{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	_, payloads, err := c.Store.Unsent(context.Background(), "event", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]schema.Event, 0, len(payloads))
	for _, p := range payloads {
		var e schema.Event
		if err := json.Unmarshal(p, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// Passed through unchanged, every reasoning token opencode reports would be
// missing from the total.
func TestOpenCodeReasoningIsAddedNotAssumedToBeInsideOutput(t *testing.T) {
	c := openCodeCtx(t, `{
		"role":"assistant","modelID":"deepseek-flash","providerID":"deepseek",
		"variant":"max","agent":"build","path":{"cwd":"/tmp/p"},"cost":0.5,
		"tokens":{"input":100,"output":20,"reasoning":300,
		          "cache":{"read":1000,"write":50}},
		"time":{"created":1780000000000}}`)

	got := collected(t, c)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	u := got[0].Usage

	if u.ReasoningTokens != 300 {
		t.Fatalf("reasoning = %d, want 300 kept for reporting", u.ReasoningTokens)
	}
	if u.OutputTokens != 320 {
		t.Fatalf("output = %d, want 320 -- reasoning belongs inside output, "+
			"which is the shape the total assumes", u.OutputTokens)
	}
	// 100 + (20+300) + 1000 + 50, matching what opencode calls the total.
	if got := u.TotalTokens(); got != 1470 {
		t.Fatalf("total = %d, want 1470", got)
	}
	if u.ReasoningTokens > u.OutputTokens {
		t.Fatal("reasoning exceeds output, so it cannot be the subset the total treats it as")
	}
}

func TestOpenCodeReadsEachResponseSeparately(t *testing.T) {
	c := openCodeCtx(t,
		`{"role":"assistant","modelID":"gpt-5.6-sol","providerID":"openai","variant":"high",
		  "agent":"build","cost":1,"tokens":{"input":10,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},
		  "time":{"created":1780000000000}}`,
		`{"role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","variant":"max",
		  "agent":"build","cost":2,"tokens":{"input":20,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},
		  "time":{"created":1780000600000}}`,
		// A user turn carries no usage and is not an event.
		`{"role":"user","tokens":{"input":0,"output":0}}`,
	)

	got := collected(t, c)
	if len(got) != 2 {
		t.Fatalf("got %d events, want one per assistant response", len(got))
	}

	models := map[string]string{}
	for _, e := range got {
		models[e.Model] = e.Effort
		if e.SessionID != "ses_1" {
			t.Errorf("session_id = %q, want the conversation they share", e.SessionID)
		}
		if e.NativeID == e.SessionID {
			t.Error("native_id equals session_id, which is how the superseded " +
				"session rollups are identified -- these must not look like them")
		}
	}
	if models["gpt-5.6-sol"] != "high" || models["deepseek-flash"] != "max" {
		t.Fatalf("model/effort pairs = %v; each response keeps its own, not the "+
			"session's last", models)
	}
	if got[0].TS.Equal(got[1].TS) {
		t.Fatal("both events share a timestamp; a session's whole life landing " +
			"on one instant is what this replaced")
	}
}

// A message opencode recorded no cost for is priced from the table; a zero it
// did record is kept, since only opencode knows a free model was free.
func TestOpenCodeKeepsAZeroCostButNotAMissingOne(t *testing.T) {
	c := openCodeCtx(t,
		`{"role":"assistant","time":{"created":1790000000000},"modelID":"m","providerID":"p",
		  "agent":"build","cost":0,"tokens":{"input":10,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		`{"role":"assistant","time":{"created":1790000001000},"modelID":"m","providerID":"p",
		  "agent":"build","tokens":{"input":20,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}`)
	events := collected(t, c)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	byInput := map[int64]*float64{}
	for _, e := range events {
		byInput[e.Usage.InputTokens] = e.NativeCostUSD
	}
	if v := byInput[10]; v == nil || *v != 0 {
		t.Errorf("recorded zero cost = %v, want an explicit 0", v)
	}
	if v := byInput[20]; v != nil {
		t.Errorf("missing cost = %v, want none so the table prices it", *v)
	}
}
