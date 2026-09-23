package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samiashi/llm-tracker/agent/internal/store"
)

// A symlinked root's cursors sit under the root as configured, where rewind
// and resync look for them, not under its target.
func TestASymlinkedRootIsRead(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	const transcript = "-Users-dev-src-app/s1.jsonl"
	line := `{"type":"assistant","requestId":"req_1","timestamp":"2026-09-22T10:00:00Z",` +
		`"message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.MkdirAll(filepath.Join(elsewhere, filepath.Dir(transcript)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, transcript), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".claude/projects")); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	res, err := (ClaudeCode{}).Collect(context.Background(), &Ctx{Store: st, MachineID: "m", Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if evs := storedEvents(t, st); len(evs) != 1 || res.Files != 1 {
		t.Fatalf("read %d files and stored %d events through a symlinked root, want 1 and 1",
			res.Files, len(evs))
	}

	cursor := filepath.Join(home, ".claude/projects", transcript)
	if offset, _, _ := st.Cursor(context.Background(), cursor); offset != int64(len(line)) {
		t.Fatalf("cursor for %s is at %d, want %d: it must be keyed on the configured root",
			cursor, offset, len(line))
	}
	if !covers(ScopeOf(ClaudeCode{}, home).CursorPrefixes, cursor) {
		t.Fatalf("rewind would not reach the cursor %s", cursor)
	}
}

// Gemini walks its own tree rather than going through walkJSONL, and needs
// the same treatment.
func TestASymlinkedGeminiRootIsRead(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	body := `{"sessionId":"s1","model":"gemini-3-pro","session_input_tokens":100,"session_output_tokens":20}`
	if err := os.MkdirAll(filepath.Join(elsewhere, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "proj", "session_1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".gemini/tmp")); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &Ctx{Store: st, MachineID: "m", Home: home}
	if _, err := (Gemini{}).Collect(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CommitPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := storedEvents(t, st); len(evs) != 1 {
		t.Fatalf("stored %d events through a symlinked root, want 1", len(evs))
	}
}
