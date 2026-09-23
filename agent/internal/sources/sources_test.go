package sources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		_, offset, size, _, err := tailJSONL(ctx, st, p, func(_ int64, line []byte) {
			var v struct{ N int }
			if json.Unmarshal(line, &v) == nil {
				seen = append(seen, v.N)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		// The caller commits the cursor, as walkJSONL does.
		if _, err := st.CommitFile(ctx, p, offset, size, nil, nil); err != nil {
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
		_, offset, size, _, err := tailJSONL(ctx, st, p, func(int64, []byte) { n++ })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitFile(ctx, p, offset, size, nil, nil); err != nil {
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
	_, payloads, err := st.Unsent(context.Background(), 1000)
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
	if _, _, _, _, err := tailJSONL(ctx, st, p, func(int64, []byte) { n++ }); err != nil {
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
	if _, _, _, _, err := tailJSONL(ctx, st, p, func(int64, []byte) { again++ }); err != nil {
		t.Fatal(err)
	}
	if again != 2 {
		t.Fatalf("re-read %d lines, want 2 -- an uncommitted file must be retried whole", again)
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

// An event is credited to whoever was signed in at its own time. While nobody
// was, that is nobody: not the account signed in before, nor the one signed
// in now.
func TestUsageIsCreditedToTheAccountSignedInAtItsTime(t *testing.T) {
	at := func(hhmm string) time.Time { return parseTS("2026-09-20T" + hhmm + ":00Z") }
	c := &Ctx{
		Accounts: map[string]*schema.Account{
			"openai":    {Provider: "openai", Ref: "openai:b"},
			"anthropic": {Provider: "anthropic", Ref: "anthropic:now"},
		},
		AccountHistory: []store.AccountWindow{
			{Provider: "openai", Ref: "openai:a", ObservedAt: at("10:00")},
			{Provider: "openai", Ref: "", ObservedAt: at("12:00")},
			{Provider: "openai", Ref: "openai:b", ObservedAt: at("14:00")},
		},
	}
	for _, tc := range []struct {
		provider, when, want string
	}{
		{"openai", "09:00", "openai:a"}, // before the first switch: the earliest known
		{"openai", "11:00", "openai:a"},
		{"openai", "13:00", ""}, // signed out
		{"openai", "15:00", "openai:b"},
		{"anthropic", "13:00", "anthropic:now"}, // no switch recorded: whoever is signed in
	} {
		if got := c.AccountRefAt(tc.provider, at(tc.when)); got != tc.want {
			t.Errorf("%s at %s: credited to %q, want %q", tc.provider, tc.when, got, tc.want)
		}
	}
}

// Harnesses write RFC3339 with and without fractional seconds, in UTC and in
// a local offset; a record whose time will not parse is dated zero, never
// guessed.
func TestTimestampsParseWithAndWithoutFractions(t *testing.T) {
	want := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-09-22T10:00:00Z":           want,
		"2026-09-22T10:00:00.123Z":       want.Add(123 * time.Millisecond),
		"2026-09-22T10:00:00.123456789Z": want.Add(123456789),
		"2026-09-22T14:00:00+04:00":      want,
		"2026-09-22T14:00:00.5+04:00":    want.Add(500 * time.Millisecond),
		"":                               {},
		"2026-09-22 10:00:00":            {},
		"not a time":                     {},
	} {
		if got := parseTS(in); !got.Equal(want) {
			t.Errorf("parseTS(%q) = %v, want %v", in, got, want)
		}
	}
}
