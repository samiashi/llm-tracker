package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A file replaced while its first line is still being written is read from
// the start once that line completes. The rewind has to be committed on its
// own: otherwise, once the replacement outgrows the old file, the next pass
// resumes at the old offset and skips the replacement's first records.
func TestAFileReplacedMidWriteIsReadFromItsStart(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := filepath.Join(home, ".claude/projects/-p/s.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := func(from, to int) string {
		var b strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&b, `{"type":"assistant","requestId":"r%d","timestamp":"2026-09-22T10:00:00Z",`+
				`"message":{"id":"m%d","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20}}}`+"\n", i, i)
		}
		return b.String()
	}
	for _, body := range []string{
		lines(1, 3),
		`{"type":"assistant","requestId":"r4"`, // replaced; its first line is mid-write
		lines(4, 8),                            // longer than the file it replaced
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (ClaudeCode{}).Collect(ctx, &Ctx{Store: st, MachineID: "m", Home: home}); err != nil {
			t.Fatal(err)
		}
	}
	if evs := storedEvents(t, st); len(evs) != 8 {
		t.Fatalf("stored %d events, want 8: the replacement's first records were skipped", len(evs))
	}
}

// Rewritten shorter, but still past the cursor, a file is shown to be new
// only by the size last recorded: resumed at the old offset, the read starts
// mid-record and loses every record before it.
func TestTailerRestartsAFileThatShrankButNotPastTheCursor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	read := func() (seen []int) {
		t.Helper()
		_, offset, size, _, err := tailJSONL(ctx, st, p, func(_ int64, line []byte) {
			var v struct{ N int }
			if json.Unmarshal(line, &v) == nil {
				seen = append(seen, v.N)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitFile(ctx, p, offset, size, nil, nil); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	// Three lines and one still being written: cursor 24, size 30.
	if err := os.WriteFile(p, []byte("{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n{\"n\":4"), 0o600); err != nil {
		t.Fatal(err)
	}
	read()
	// Rewritten at 26 bytes: shorter than 30, longer than 24.
	if err := os.WriteFile(p, []byte("{\"n\":10}\n{\"n\":20}\n{\"n\":7}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(); !slices.Equal(got, []int{10, 20, 7}) {
		t.Fatalf("read %v from the rewritten file, want all of it", got)
	}
}
