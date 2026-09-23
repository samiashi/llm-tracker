package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/collect"
	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// agentRun runs the CLI as a user would and returns what it printed. Callers
// point HOME at a directory of their own first: nothing here may read or
// write the real one.
func agentRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if home, _ := os.UserHomeDir(); !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("HOME is %s; set it to a temporary directory before running the agent", home)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldOut }()
	os.Args = append([]string{"llm-tracker-agent"}, args...)
	os.Stdout = w

	printed := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		printed <- string(b)
	}()
	runErr := run()
	_ = w.Close()
	return <-printed, runErr
}

// decodeBatch reads an upload the way the server does.
func decodeBatch(t *testing.T, r *http.Request) schema.Batch {
	t.Helper()
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return schema.Batch{}
		}
		body = zr
	}
	var b schema.Batch
	_ = json.NewDecoder(body).Decode(&b)
	return b
}

// seed writes events into the store in dataDir, as a pass would.
func seed(t *testing.T, dataDir string, events ...schema.Event) {
	t.Helper()
	st, err := store.Open(filepath.Join(dataDir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs := make([]store.Record, 0, len(events))
	for _, e := range events {
		recs = append(recs, store.Record{ID: e.ID, TS: e.TS, TotalTokens: e.Usage.TotalTokens(),
			Collector: 1, Payload: e})
	}
	if _, err := st.CommitFile(context.Background(), "", 0, 0, recs, nil); err != nil {
		t.Fatal(err)
	}
}

func event(id string, ts time.Time, model string) schema.Event {
	return schema.Event{V: schema.Version, ID: id, NativeID: id, Source: schema.SourceClaudeCode,
		TS: ts, MachineID: "m", Model: model, Usage: schema.Usage{InputTokens: 10}}
}

func health(t *testing.T, dataDir string) store.Health {
	t.Helper()
	st, err := store.Open(filepath.Join(dataDir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err := st.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Unrecorded, `status` says "never" straight after a successful manual sync.
func TestAManualSyncIsRecordedForStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := decodeBatch(t, r)
		if refuse.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid token"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.IngestAck{ServerVersion: version, EventsReceived: len(b.Events)})
	}))
	defer srv.Close()
	if err := config.Save(dir, config.Config{ServerURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed(t, dir, event("a", now, "m"), event("b", now, "m"), event("c", now, "m"))

	if _, err := agentRun(t, "sync", "-data", dir); err != nil {
		t.Fatal(err)
	}
	if h := health(t, dir); h.LastSyncAt.IsZero() || h.LastUploaded != 3 || h.LastSyncErr != "" {
		t.Fatalf("after a good sync: last sync %v, %d uploaded, error %q -- want it recorded",
			h.LastSyncAt, h.LastUploaded, h.LastSyncErr)
	}

	refuse.Store(true)
	if _, err := agentRun(t, "sync", "-data", dir); err == nil {
		t.Fatal("sync succeeded against a refusal")
	}
	if h := health(t, dir); !strings.Contains(h.LastSyncErr, "401") {
		t.Fatalf("after a refused sync the recorded error is %q, want the refusal", h.LastSyncErr)
	}
}

func TestSyncSaysWhatTheServerRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	const floor = "2026-01-01"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ack := schema.IngestAck{ServerVersion: version, RetentionFloor: floor, RetentionEnforced: true}
		for _, e := range decodeBatch(t, r).Events {
			switch {
			case e.Model == "implausible":
				ack.EventsRejected++
			case e.TS.UTC().Format("2006-01-02") < floor:
				ack.EventsReceived++
				ack.EventsSkipped++
			default:
				ack.EventsReceived++
			}
		}
		_ = json.NewEncoder(w).Encode(ack)
	}))
	defer srv.Close()
	if err := config.Save(dir, config.Config{ServerURL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed(t, dir,
		event("old", time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC), "m"),
		event("bad", now, "implausible"),
		event("good", now, "m"))

	out, err := agentRun(t, "sync", "-data", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rejected 1 of those events", "refused 1 events that predate"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync printed %q, want it to say %q", out, want)
		}
	}
}

// Saying only how to stop the collector leaves collection off.
func TestALockedDataDirSaysHowToRestartTheCollector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	lock, err := store.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release() //nolint:errcheck

	_, err = agentRun(t, "resend", "-data", dir)
	if err == nil {
		t.Fatal("resend ran while another agent held the directory")
	}
	for _, want := range []string{
		"launchctl bootout gui/$(id -u)/io.github.samiashi.llm-tracker",
		"llm-tracker-agent install -data " + dir,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// `sync` also needs the collector stopped, so "run sync" alone leaves the
// re-queued archive waiting on a collector nobody restarts.
func TestResendSaysHowTheQueueIsDelivered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	seed(t, dir, event("a", time.Now(), "m"))

	out, err := agentRun(t, "resend", "-data", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "llm-tracker-agent install -data "+dir) {
		t.Errorf("resend printed %q, want how to start the collector again", out)
	}
}

// A probe that reads back one page of the store undercounts any real history.
func TestProbeCountsEveryRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "-Users-dev-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	line := func(i int, ts string, input int) {
		fmt.Fprintf(&b, `{"type":"assistant","requestId":"r%d","sessionId":"s","timestamp":%q,`+
			`"message":{"id":"m%d","model":"claude-sonnet-4-5","usage":{"input_tokens":%d}}}`+"\n",
			i, ts, i, input)
	}
	for i := range 5000 {
		line(i, "2026-09-20T10:00:00Z", 10)
	}
	// The latest response carries almost all the tokens, so a probe that stops
	// at the first 5,000 rows shows it. It streams in two lines: two records
	// parsed, one event stored.
	line(5000, "2026-09-21T10:00:00Z", 10)
	line(5000, "2026-09-21T10:00:00Z", 1_000_000_000)
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := agentRun(t, "probe", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"events parsed: 5002 (5001 distinct)", "tokens: 1.0B"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe did not report %q:\n%s", want, out)
		}
	}
}

func TestCommandsDefaultToTheInstalledCollectorsDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeGH(t)
	tr := newTracker(t, schema.EnrolledTokenPrefix+"issued")
	installed := t.TempDir()

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>Label</key><string>io.github.samiashi.llm-tracker</string>
  <key>ProgramArguments</key>
  <array><string>/x/llm-tracker-agent</string><string>run</string>
    <string>-data</string><string>` + installed + `</string></array>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agents, "io.github.samiashi.llm-tracker.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := agentRun(t, "enroll", "-server", tr.URL); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.Load(installed); cfg.ServerURL != tr.URL {
		t.Fatalf("installed collector's config has server %q; enroll wrote elsewhere", cfg.ServerURL)
	}
	if _, err := os.Stat(filepath.Join(home, ".llm-tracker", "config.json")); err == nil {
		t.Error("enroll wrote ~/.llm-tracker although the collector runs on another directory")
	}
}

// resync deletes before it re-reads, so it warns for every harness that can
// lose records -- not only Claude Code: Codex and opencode sessions, Cline
// tasks and Continue logs can all be deleted by the user.
func TestResyncWarnsForEveryHarnessThatCanLoseRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, source := range []string{"claude_code", "cowork", "codex", "opencode", "cline", "continue"} {
		out, err := agentRun(t, "resync", "-data", t.TempDir(), "-source", source, "-yes")
		if err != nil {
			t.Fatalf("resync %s: %v", source, err)
		}
		if !strings.Contains(out, "does not keep every record") {
			t.Errorf("resync %s gave no warning that deleted records cannot be re-read:\n%s", source, out)
		}
	}
}

// One pass that reads new data and finds no usage is ordinary -- a Codex turn
// writes its tool output before the usage line that closes it -- so the alarm
// waits for the silence to repeat. A pass with nothing new keeps the count;
// a pass that finds usage clears it.
func TestASourceIsReportedSilentOnlyWhenItStaysSilent(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	pass := func(s collect.SourceSummary) *collect.Summary {
		return &collect.Summary{PerSource: map[schema.Source]collect.SourceSummary{schema.SourceCodex: s}}
	}
	silentPass := pass(collect.SourceSummary{Available: true, BytesRead: 40_000})
	idle := pass(collect.SourceSummary{Available: true})
	found := pass(collect.SourceSummary{Available: true, BytesRead: 10_000, Found: 2})

	sl := silence{}
	for i, p := range []*collect.Summary{silentPass, idle, silentPass, found, silentPass, silentPass} {
		sl.report(p, log)
		if strings.Contains(buf.String(), "format may have changed") {
			t.Fatalf("alarm after pass %d; silence never reached %d passes in a row", i+1, silenceAlarmPasses)
		}
	}
	sl.report(silentPass, log)
	if !strings.Contains(buf.String(), "format may have changed") {
		t.Fatalf("no alarm after %d silent passes", silenceAlarmPasses)
	}
}
