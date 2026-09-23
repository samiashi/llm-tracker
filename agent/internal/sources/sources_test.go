package sources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

func TestTailerLeavesPartialTrailingLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(p, []byte("{\"n\":1}\n{\"n\":2}\n{\"n\":3"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	var seen []int
	read := func() {
		_, offset, size, err := tailJSONL(ctx, st, p, func(_ int64, line []byte) {
			var v struct{ N int }
			if json.Unmarshal(line, &v) == nil {
				seen = append(seen, v.N)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		// The caller commits the cursor, as walkJSONL does.
		if _, err := st.CommitFile(ctx, p, offset, size, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	read()
	if len(seen) != 2 {
		t.Fatalf("read %v, want only the two complete lines", seen)
	}

	// The rest of line 3 arrives; it must now be read exactly once.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("}\n")
	f.Close()

	read()
	if len(seen) != 3 || seen[2] != 3 {
		t.Fatalf("read %v, want the completed third line appended once", seen)
	}
}

func TestTailerRestartsOnTruncation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	os.WriteFile(p, []byte("{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n"), 0o600)

	st, _ := store.Open(filepath.Join(dir, "t.db"))
	defer st.Close()
	ctx := context.Background()

	count := func() int {
		n := 0
		_, offset, size, err := tailJSONL(ctx, st, p, func(int64, []byte) { n++ })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitFile(ctx, p, offset, size, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 3 {
		t.Fatalf("first pass read %d, want 3", got)
	}
	if got := count(); got != 0 {
		t.Fatalf("second pass read %d, want 0 -- nothing was appended", got)
	}

	os.WriteFile(p, []byte("{\"n\":9}\n"), 0o600) // replaced, now shorter
	if got := count(); got != 1 {
		t.Fatalf("after truncation read %d, want 1 -- the cursor must reset", got)
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

func TestQuotaBucketKeepsPeak(t *testing.T) {
	c := &Ctx{}
	for _, pct := range []float64{12, 61, 40} {
		c.emitQuota(schema.QuotaSample{ID: "bucket-1", UsedPercent: pct})
	}
	c.emitQuota(schema.QuotaSample{ID: "bucket-2", UsedPercent: 5})

	_, quota := c.Drain()
	if len(quota) != 2 {
		t.Fatalf("got %d samples, want 2 buckets", len(quota))
	}
	if quota[0].UsedPercent != 61 {
		t.Fatalf("bucket kept %.0f%%, want the peak 61%%", quota[0].UsedPercent)
	}
}

// An adapter with no roots is never Available, so it never runs.
func TestEveryAdapterDeclaresRoots(t *testing.T) {
	for _, a := range All() {
		if len(a.Roots()) == 0 {
			t.Errorf("adapter %s declares no roots", a.Name())
		}
	}
}

// A covered todo entry reports its harness as both supported and missing.
// Blocked entries are exempt; see ScanUnknown.
func TestAdapterRootsSuppressCandidates(t *testing.T) {
	covered := coveredRoots()
	for _, cand := range candidates {
		if cand.status == schema.StatusBlocked {
			continue
		}
		if isCovered(cand.rel, covered) {
			t.Errorf("%q is listed as unsupported but an adapter covers it", cand.rel)
		}
	}
}

func TestBlockedCandidatesSurviveCoverage(t *testing.T) {
	covered := coveredRoots()
	var cursor *candidate
	for i := range candidates {
		if candidates[i].rel == "Library/Application Support/Cursor" {
			cursor = &candidates[i]
		}
	}
	if cursor == nil {
		t.Fatal("the Cursor entry is gone; it documents why Cursor cannot be read")
	}
	if cursor.status != schema.StatusBlocked || cursor.note == "" {
		t.Fatal("the Cursor entry must stay blocked and carry its reason")
	}
	// The exemption is load-bearing only while an adapter reads under here.
	if !isCovered(cursor.rel, covered) {
		t.Skip("no adapter reads from under the Cursor directory any more")
	}
}

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

func TestKimiIgnoresSessionScopedRecords(t *testing.T) {
	events := collectFile(t, Kimi{}, ".kimi/sessions/s1/run/wire.jsonl",
		`{"type":"usage.record","scope":"session","request_id":"s1","usage":{"inputOther":999,"output":999}}
{"type":"usage.record","scope":"turn","request_id":"t1","usage":{"inputOther":10,"output":5}}
`)
	if len(events) != 1 || events[0].NativeID != "t1" {
		t.Fatalf("got %+v, want only the turn-scoped record", events)
	}
}

// collectFile writes body at home/rel, runs the adapter over that home, and
// returns what it stored: Collect commits as it goes, so Drain is empty after.
func collectFile(t *testing.T, a Adapter, rel, body string) []schema.Event {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := a.Collect(context.Background(), &Ctx{Store: st, MachineID: "m", Home: home}); err != nil {
		t.Fatal(err)
	}
	_, payloads, err := st.Unsent(context.Background(), "event", 1000)
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

// A file whose rows fail to store must not advance its cursor, or the bytes
// are marked read and their events are lost for good.
func TestCursorDoesNotAdvanceWithoutTheRows(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(p, []byte("{\"n\":1}\n{\"n\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Read without committing, as an interrupted pass would.
	n := 0
	if _, _, _, err := tailJSONL(ctx, st, p, func(int64, []byte) { n++ }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("read %d lines, want 2", n)
	}

	offset, _, err := st.Cursor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Fatalf("cursor advanced to %d without a commit; those events would be lost", offset)
	}

	// A second pass therefore sees the same lines again.
	again := 0
	if _, _, _, err := tailJSONL(ctx, st, p, func(int64, []byte) { again++ }); err != nil {
		t.Fatal(err)
	}
	if again != 2 {
		t.Fatalf("re-read %d lines, want 2 -- an uncommitted file must be retried whole", again)
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
		_, payloads, err := st.Unsent(context.Background(), "event", 100)
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
	ids, _, err := st.Unsent(context.Background(), "event", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 {
		t.Fatalf("stored %d events, want 4 -- records sharing a timestamp must "+
			"stay distinct", len(ids))
	}
}

func TestSourcesNeedingBackfill(t *testing.T) {
	if got := SourcesNeedingBackfill(0); len(got) == 0 {
		t.Fatal("upgrading from nothing should name the affected sources")
	}
	if got := SourcesNeedingBackfill(CollectorVersion); got != nil {
		t.Errorf("an up-to-date collector wants a backfill of %v", got)
	}
	if got := SourcesNeedingBackfill(CollectorVersion + 1); got != nil {
		t.Errorf("a newer stored version wants a backfill of %v", got)
	}

	// Spanning several versions accumulates them without repeats; codex is
	// named by more than one.
	got := SourcesNeedingBackfill(1)
	seen := map[schema.Source]int{}
	for _, s := range got {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("%s listed %d times", s, n)
		}
	}
	if seen[schema.SourceCodex] != 1 {
		t.Errorf("codex should appear exactly once across versions 2..%d", CollectorVersion)
	}

	// Every named source must actually exist, or a rename silently stops the
	// backfill it was meant to trigger.
	for v, srcs := range backfillOnUpgrade {
		if v > CollectorVersion {
			t.Errorf("version %d is in the table but above CollectorVersion", v)
		}
		for _, s := range srcs {
			if _, ok := Lookup(string(s)); !ok {
				t.Errorf("version %d names %q, which is not a registered source", v, s)
			}
		}
	}
}

func TestAnIdentityChangePurgesWhatItReplaces(t *testing.T) {
	if got := SourcesNeedingPurge(6); !slices.Contains(got, schema.SourceOpenCode) {
		t.Fatalf("SourcesNeedingPurge(6) = %v, want opencode: version 7 reads it per response", got)
	}
	if got := SourcesNeedingPurge(8); !slices.Equal(got, []schema.Source{schema.SourceCodex}) {
		t.Fatalf("SourcesNeedingPurge(8) = %v, want codex: version 9 keys its usage on content", got)
	}
	if n := SourcesNeedingPurge(CollectorVersion); n != nil {
		t.Fatalf("a current collector purges %v; it must purge nothing", n)
	}
}

func TestEverySourcePurgedOnUpgradeKeepsItsHistory(t *testing.T) {
	for v, srcs := range purgeOnUpgrade {
		for _, s := range srcs {
			a, ok := Lookup(string(s))
			if !ok {
				t.Errorf("version %d purges %q, which is not a registered source", v, s)
				continue
			}
			if !KeepsHistory(a) {
				t.Errorf("version %d purges %s, whose history does not outlive our "+
					"archive; match old rows to new ones in dedupeOnUpgrade instead", v, s)
			}
		}
	}
	for _, s := range []schema.Source{schema.SourceClaudeCode, schema.SourceCowork} {
		if a, _ := Lookup(string(s)); KeepsHistory(a) {
			t.Errorf("%s deletes its transcripts after thirty days; it must not claim to keep them", s)
		}
	}
}

func TestGeminiKeepsTheReleasedIDForASessionWithoutOne(t *testing.T) {
	events := collectAndCommit(t, Gemini{}, ".gemini/tmp/project-a/session.json",
		`{"model":"gemini-3-pro","session_input_tokens":100,"session_output_tokens":20}`)
	if len(events) != 1 || events[0].NativeID != "session" {
		t.Fatalf("got %+v, want one event with the released id %q", events, "session")
	}
}

func TestUndecodableLinesAreCounted(t *testing.T) {
	for _, tc := range []struct {
		a   Adapter
		rel string
	}{
		{ZCode{}, ".zcode/sessions/s.jsonl"},
		{DeepSeekHarness{}, ".dsh/s.jsonl"},
		{Copilot{}, ".copilot/session-state/s.jsonl"},
	} {
		t.Run(string(tc.a.Name()), func(t *testing.T) {
			home := t.TempDir()
			p := filepath.Join(home, tc.rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("not json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			res, err := tc.a.Collect(context.Background(), &Ctx{Store: st, MachineID: "m", Home: home})
			if err != nil {
				t.Fatal(err)
			}
			if res.Unparsed != 1 {
				t.Fatalf("Unparsed = %d, want 1", res.Unparsed)
			}
		})
	}
}

// collectAndCommit is collectFile for an adapter that leaves its events to the
// collector's CommitPending rather than committing them itself.
func collectAndCommit(t *testing.T, a Adapter, rel, body string) []schema.Event {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &Ctx{Store: st, MachineID: "m", Home: home}
	if _, err := a.Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CommitPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	return storedEvents(t, st)
}
