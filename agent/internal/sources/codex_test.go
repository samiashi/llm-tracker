package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// tokenCount is a token_count line as rollouts write it: the response's own
// usage and the thread's running total.
func tokenCount(ts string, ordinal int, last, total int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","ordinal":%d,`+
		`"payload":{"type":"token_count","info":{`+
		`"last_token_usage":{"input_tokens":%d,"output_tokens":10,"total_tokens":%d},`+
		`"total_token_usage":{"input_tokens":%d,"output_tokens":10,"total_tokens":%d}}}}`,
		ts, ordinal, last, last+10, total, total+10)
}

func usageRecord(ts string, ordinal int, response string, in int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"token_usage_record","ordinal":%d,`+
		`"payload":{"response_id":%q,"usage":{"input_tokens":%d,"output_tokens":10,"total_tokens":%d}}}`,
		ts, ordinal, response, in, in+10)
}

func sessionMeta(ordinal int, id, session, source, extra string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-20T10:00:00Z","type":"session_meta","ordinal":%d,`+
		`"payload":{"id":%q,"session_id":%q,"thread_source":%q%s}}`, ordinal, id, session, source, extra)
}

// codexHome is a fake home and store holding rollouts, keyed by their path
// under the home directory.
type codexHome struct {
	home string
	st   *store.Store
}

func newCodexHome(t *testing.T) *codexHome {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &codexHome{home: t.TempDir(), st: st}
}

// write appends lines to the file at rel, creating it if needed.
func (h *codexHome) write(t *testing.T, rel string, lines ...string) {
	t.Helper()
	p := filepath.Join(h.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *codexHome) collect(t *testing.T, c *Ctx) {
	t.Helper()
	if c == nil {
		c = &Ctx{}
	}
	c.Store, c.MachineID, c.Home = h.st, "m", h.home
	if _, err := (Codex{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func storedEvents(t *testing.T, st *store.Store) []schema.Event {
	t.Helper()
	_, payloads, err := st.Unsent(context.Background(), 10000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]schema.Event, 0, len(payloads))
	for _, raw := range payloads {
		var e schema.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func totalOf(events []schema.Event) int64 {
	var n int64
	for _, e := range events {
		n += e.Usage.TotalTokens()
	}
	return n
}

const (
	parentRollout = "rollout-2026-09-20T10-00-00-019ef8d7-aaaa-bbbb-cccc-000000000001.jsonl"
	childRollout  = "rollout-2026-09-20T11-00-00-019ef8d7-aaaa-bbbb-cccc-000000000002.jsonl"
	liveDir       = ".codex/sessions/2026/09/20/"
	archiveDir    = ".codex/archived_sessions/"
)

func TestCodexCountsARestatedResponseOnce(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout,
		tokenCount("2026-09-20T10:00:03Z", 3, 1000, 1000),
		tokenCount("2026-09-20T10:00:04Z", 4, 1000, 1000), // the same response, restated
		tokenCount("2026-09-20T10:00:06Z", 6, 2000, 3000),
	)
	h.collect(t, nil)

	evs := storedEvents(t, h.st)
	if len(evs) != 2 || totalOf(evs) != 1010+2010 {
		t.Fatalf("stored %d events / %d tokens, want 2 / %d", len(evs), totalOf(evs), 1010+2010)
	}
}

func TestCodexCountsAForksCopyOfItsParentOnce(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout,
		sessionMeta(0, "p", "p", "user", ""),
		tokenCount("2026-09-20T10:00:03Z", 3, 1000, 1000),
		tokenCount("2026-09-20T10:00:06Z", 6, 2000, 3000),
	)
	h.write(t, liveDir+childRollout,
		sessionMeta(0, "c", "p", "subagent", `,"parent_thread_id":"p"`),
		tokenCount("2026-09-20T11:00:00Z", 3, 1000, 1000), // copied from the parent
		tokenCount("2026-09-20T11:00:00Z", 6, 2000, 3000), // copied from the parent
		tokenCount("2026-09-20T11:00:09Z", 9, 500, 3500),  // the child's own response
	)
	h.collect(t, nil)

	evs := storedEvents(t, h.st)
	if len(evs) != 3 || totalOf(evs) != 1010+2010+510 {
		t.Fatalf("stored %d events / %d tokens, want 3 / %d", len(evs), totalOf(evs), 1010+2010+510)
	}
}

func TestCodexDatesAResponseFromItsOriginalNotAForksCopy(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, archiveDir+parentRollout,
		sessionMeta(0, "p", "p", "user", ""),
		tokenCount("2026-09-18T10:00:03Z", 3, 1000, 1000),
	)
	h.write(t, liveDir+childRollout,
		sessionMeta(0, "c", "c", "user", `,"forked_from_id":"p"`),
		tokenCount("2026-09-20T11:00:00Z", 3, 1000, 1000), // copied at fork time
	)
	h.collect(t, nil)

	evs := storedEvents(t, h.st)
	if len(evs) != 1 {
		t.Fatalf("stored %d events, want the one response", len(evs))
	}
	if want := parseTS("2026-09-18T10:00:03Z"); !evs[0].TS.Equal(want) {
		t.Fatalf("response dated %s, want its original %s", evs[0].TS, want)
	}
}

func TestCodexKeysOnPositionWithoutARunningTotal(t *testing.T) {
	line := func(ordinal int) string {
		return fmt.Sprintf(`{"timestamp":"2026-09-20T10:00:%02dZ","type":"event_msg","ordinal":%d,`+
			`"payload":{"type":"token_count","info":{"last_token_usage":`+
			`{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}`, ordinal, ordinal)
	}
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout, line(3), line(5))
	h.collect(t, nil)

	if evs := storedEvents(t, h.st); len(evs) != 2 {
		t.Fatalf("stored %d events, want 2 -- identical usage is not the same response", len(evs))
	}
}

// A rollout resumed across a Codex upgrade reports its early responses only
// through token_count; they count whether one pass reads the file or two.
func TestCodexKeepsResponsesReportedBeforeTheFirstUsageRecord(t *testing.T) {
	lines := []string{
		tokenCount("2026-09-20T10:00:03Z", 3, 1000, 1000),
		tokenCount("2026-09-20T10:00:05Z", 5, 2000, 3000),
		usageRecord("2026-09-20T10:01:07Z", 7, "resp_C", 4000),
		tokenCount("2026-09-20T10:01:08Z", 8, 4000, 7000), // resp_C restated
	}
	const want = 1010 + 2010 + 4010

	onePass := newCodexHome(t)
	onePass.write(t, liveDir+parentRollout, lines...)
	onePass.collect(t, nil)
	if got := totalOf(storedEvents(t, onePass.st)); got != want {
		t.Errorf("one pass stored %d tokens, want %d", got, want)
	}

	twoPasses := newCodexHome(t)
	twoPasses.write(t, liveDir+parentRollout, lines[:2]...)
	twoPasses.collect(t, nil)
	twoPasses.write(t, liveDir+parentRollout, lines[2:]...)
	twoPasses.collect(t, nil)
	if got := totalOf(storedEvents(t, twoPasses.st)); got != want {
		t.Errorf("two passes stored %d tokens, want %d", got, want)
	}
}

// The parent's copied header must not relabel a rollout, even when it arrives
// in a later pass.
func TestCodexLabelsARolloutFromItsOwnHeader(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+childRollout,
		sessionMeta(0, "c", "root", "subagent", `,"originator":"codex_cli_rs","model_provider":"openai"`))
	h.collect(t, nil)
	h.write(t, liveDir+childRollout,
		sessionMeta(1, "p", "p", "user", `,"originator":"Codex Desktop","model_provider":"oss"`),
		tokenCount("2026-09-20T10:00:05Z", 5, 1000, 1000),
	)
	h.collect(t, nil)

	evs := storedEvents(t, h.st)
	if len(evs) != 1 {
		t.Fatalf("stored %d events, want 1", len(evs))
	}
	e := evs[0]
	if !e.IsSubagent || e.Surface != schema.SurfaceCLI || e.Endpoint != "" || e.SessionID != "root" {
		t.Fatalf("got subagent=%v surface=%q endpoint=%q session=%q; the parent's copied "+
			"header relabelled the rollout", e.IsSubagent, e.Surface, e.Endpoint, e.SessionID)
	}
}

// Without the header's session, a token_count response is missing from every
// per-session view.
func TestCodexTokenCountResponsesCarryTheirSession(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout,
		sessionMeta(0, "thread-1", "session-1", "subagent", ""),
		tokenCount("2026-09-20T10:00:05Z", 5, 1000, 1000),
	)
	h.collect(t, nil)

	if evs := storedEvents(t, h.st); len(evs) != 1 || evs[0].SessionID != "session-1" {
		t.Fatalf("got %+v, want one event in session-1", evs)
	}
}

func TestCodexAttributesUsageToTheAccountActiveAtTheTime(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout, tokenCount("2026-09-20T10:00:05Z", 5, 100, 100))
	h.collect(t, &Ctx{
		Accounts: map[string]*schema.Account{"openai": {Provider: "openai", Ref: "openai:work"}},
		AccountHistory: []store.AccountWindow{
			{Provider: "openai", Ref: "openai:personal", ObservedAt: parseTS("2026-09-01T00:00:00Z")},
			{Provider: "openai", Ref: "openai:work", ObservedAt: parseTS("2026-09-21T00:00:00Z")},
		},
	})

	if evs := storedEvents(t, h.st); len(evs) != 1 || evs[0].AccountRef != "openai:personal" {
		t.Fatalf("got %+v, want the usage credited to the account active on the day", evs)
	}
}

// Without an endpoint, a local or third-party provider's usage is priced at
// OpenAI's rates.
func TestCodexEndpointFollowsTheModelProvider(t *testing.T) {
	for provider, want := range map[string]string{
		"openai":   "",
		"oss":      "ollama",
		"ollama":   "ollama",
		"lmstudio": "lmstudio",
		"Azure":    "azure",
	} {
		t.Run(provider, func(t *testing.T) {
			h := newCodexHome(t)
			h.write(t, liveDir+parentRollout,
				sessionMeta(0, "p", "p", "user", fmt.Sprintf(`,"model_provider":%q`, provider)),
				tokenCount("2026-09-20T10:00:05Z", 5, 1000, 1000),
			)
			h.collect(t, nil)
			if evs := storedEvents(t, h.st); len(evs) != 1 || evs[0].Endpoint != want {
				t.Fatalf("got %+v, want endpoint %q", evs, want)
			}
		})
	}
}

// Server migration 00016 tells the two apart by shape: it recognises the new
// key by 'tc:' and deletes Codex rows matching '*.jsonl#*'.
func TestCodexTokenCountIDsCannotBeMistakenForOrdinalKeys(t *testing.T) {
	h := newCodexHome(t)
	h.write(t, liveDir+parentRollout, tokenCount("2026-09-20T10:00:05Z", 5, 1000, 1000))
	h.collect(t, nil)

	evs := storedEvents(t, h.st)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].NativeID, "tc:") || strings.Contains(evs[0].NativeID, ".jsonl#") {
		t.Fatalf("native id %q: want a tc: key", evs[0].NativeID)
	}
}

func TestCodexSubtractsCachedFromInput(t *testing.T) {
	fc := fileCtx{Model: "gpt-6-astra", Provider: "openai"}
	ev, ok := codexEvent(&Ctx{MachineID: "m"}, &fc, "acct", "resp_1", "sess",
		codexTokenUsage{InputTokens: 44094, CachedInputTokens: 37120, OutputTokens: 116},
		parseTS("2026-09-21T08:17:02Z"))
	if !ok {
		t.Fatal("expected event")
	}
	if ev.Usage.InputTokens != 44094-37120 {
		t.Fatalf("input = %d, want %d -- cached tokens must not be counted twice",
			ev.Usage.InputTokens, 44094-37120)
	}
	if ev.Usage.CacheReadTokens != 37120 {
		t.Fatalf("cache read = %d, want 37120", ev.Usage.CacheReadTokens)
	}
}

// Archiving moves a rollout to archived_sessions/, which is read too; an id
// that changed with the move would count the session twice.
func TestCodexEventIdSurvivesArchiving(t *testing.T) {
	const line = `{"timestamp":"2026-09-20T10:00:00Z","type":"event_msg","ordinal":7,` +
		`"payload":{"type":"token_count","info":{"last_token_usage":` +
		`{"input_tokens":1000,"output_tokens":200,"total_tokens":1200},` +
		`"total_token_usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}}}`

	collect := func(t *testing.T, rel string) []schema.Event {
		t.Helper()
		home := t.TempDir()
		dir := filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		name := "rollout-2026-09-20T10-00-00-019ef8d7-aaaa-bbbb-cccc-ddddeeeeffff.jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })

		c := &Ctx{Store: st, MachineID: "m", Home: home}
		if _, err := (Codex{}).Collect(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		_, payloads, err := st.Unsent(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]schema.Event, 0, len(payloads))
		for _, raw := range payloads {
			var e schema.Event
			if json.Unmarshal(raw, &e) == nil {
				out = append(out, e)
			}
		}
		return out
	}

	live := collect(t, ".codex/sessions/2026/09/20")
	archived := collect(t, ".codex/archived_sessions")

	if len(live) != 1 || len(archived) != 1 {
		t.Fatalf("got %d live and %d archived events, want 1 each", len(live), len(archived))
	}
	if live[0].ID != archived[0].ID {
		t.Fatalf("archiving changed the event id: %s -> %s (native %q -> %q); "+
			"the session would be counted twice",
			live[0].ID, archived[0].ID, live[0].NativeID, archived[0].NativeID)
	}
	if strings.Contains(archived[0].NativeID, string(os.PathSeparator)) {
		t.Errorf("NativeID carries a filesystem path: %q", archived[0].NativeID)
	}
}
