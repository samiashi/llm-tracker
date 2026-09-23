package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/collect"
	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/schema"
)

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
