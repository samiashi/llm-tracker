package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
