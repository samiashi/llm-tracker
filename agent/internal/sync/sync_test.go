package sync

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/store"
	"github.com/samiashi/llm-tracker/schema"
)

// jsonKeys lists the keys a type can put on the wire, following
// encoding/json's rule for embedded structs: their fields are promoted, and a
// shallower field of the same name hides a deeper one.
func jsonKeys(t reflect.Type) []string {
	depth := map[string]int{}
	var walk func(t reflect.Type, d int)
	walk = func(t reflect.Type, d int) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type, d+1)
				continue
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if old, ok := depth[name]; !ok || d < old {
				depth[name] = d
			}
		}
	}
	walk(t, 0)
	out := make([]string, 0, len(depth))
	for k := range depth {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// A field added to wireBatch alone reaches the wire unseen by the privacy test
// and the server's decoder.
func TestWireBatchIsSchemaBatch(t *testing.T) {
	got := jsonKeys(reflect.TypeOf(wireBatch{}))
	want := jsonKeys(reflect.TypeOf(schema.Batch{}))
	if !slices.Equal(got, want) {
		t.Fatalf("wireBatch sends %v, schema.Batch declares %v -- add the field "+
			"to schema.Batch, not to the agent's copy", got, want)
	}
}

// The pre-encoded events must be what goes out, not the embedded typed slice
// they shadow, and the server must decode every field the agent set.
func TestWireBatchRoundTripsIntoSchemaBatch(t *testing.T) {
	b := wireBatch{
		Batch: schema.Batch{
			V: schema.Version, MachineID: "m", AgentVersion: "v1.0.0",
			Accounts: []schema.Account{{Ref: "anthropic:a", Provider: "anthropic",
				Email: "dev@example.com", PlanType: "max"}},
		},
		Events: []json.RawMessage{json.RawMessage(`{"id":"e1"}`)},
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var got schema.Batch
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != "e1" {
		t.Fatalf("events = %+v, want the pre-encoded row", got.Events)
	}
	if !reflect.DeepEqual(got.Accounts, b.Accounts) {
		t.Fatalf("accounts = %+v, want %+v", got.Accounts, b.Accounts)
	}
}

// The agent reads the reply through the same type the server writes.
func TestIngestDecodesTheServersAck(t *testing.T) {
	want := schema.IngestAck{
		ServerVersion: "v1.4.0", EventsReceived: 1, EventsSkipped: 1,
		RetentionFloor: "2026-01-01", RetentionEnforced: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := New(srv.URL, "t", "v1.4.0", quietLog())
	got, err := c.ingest(context.Background(), wireBatch{Events: []json.RawMessage{json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if got.EventsSkipped != 1 || got.RetentionFloor != want.RetentionFloor ||
		got.RetentionEnforced == nil || !*got.RetentionEnforced {
		t.Fatalf("ack = %+v, want %+v", got, want)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// storeWith opens a store holding n unsent events.
func storeWith(t *testing.T, n int) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	recs := make([]store.Record, n)
	for i := range recs {
		id := fmt.Sprintf("e%d", i)
		recs[i] = store.Record{ID: id, TS: time.Now(), TotalTokens: 10, Collector: 1,
			Payload: schema.Event{ID: id, TS: time.Now()}}
	}
	if _, err := st.CommitFile(context.Background(), "", 0, 0, recs, nil, nil); err != nil {
		t.Fatal(err)
	}
	return st
}

func unsent(t *testing.T, st *store.Store) int {
	t.Helper()
	ids, _, err := st.Unsent(context.Background(), "event", 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// notAnAck answers 2xx, or redirects to something that does, without
// acknowledging the batch: the dashboard's HTML, a login page, a JSON document
// that is not an ack, an ack for a batch of a different size.
var notAnAck = map[string]http.HandlerFunc{
	"html page": func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><title>dashboard</title>")
	},
	"redirect to a login page": func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	},
	"json that is not an ack": func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	},
	"an ack for a different batch": func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(schema.IngestAck{ServerVersion: "v1.0.0", EventsReceived: 1})
	},
}

func TestOnlyAnAcknowledgementMarksABatchSent(t *testing.T) {
	for name, h := range notAnAck {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			st := storeWith(t, 3)
			c := New(srv.URL, "t", "v1.0.0", quietLog())
			if _, err := c.Push(context.Background(), st, "m"); err == nil {
				t.Fatal("push succeeded against a reply that acknowledged nothing")
			}
			if n := unsent(t, st); n != 3 {
				t.Fatalf("%d rows still queued, want 3 -- they were marked sent without an ack", n)
			}
		})
	}
}

// A proxy may gzip even a small reply. The agent must read it, or a refusal
// in it goes unseen and refused rows are marked sent.
func TestAGzippedAckIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b schema.Batch
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			body = zr
		}
		_ = json.NewDecoder(body).Decode(&b)
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_ = json.NewEncoder(zw).Encode(schema.IngestAck{
			ServerVersion: "v1.0.0", EventsReceived: len(b.Events), EventsSkipped: len(b.Events),
			RetentionFloor: "2999-01-01", RetentionEnforced: true,
		})
		_ = zw.Close()
	}))
	defer srv.Close()

	st := storeWith(t, 3)
	c := New(srv.URL, "t", "v1.0.0", quietLog())
	stats, err := c.Push(context.Background(), st, "m")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 3 || stats.Retired != 3 {
		t.Fatalf("skipped=%d retired=%d, want 3/3 -- the refusal was not read", stats.Skipped, stats.Retired)
	}
}

// Refused rows are re-offered on what the server says it accepts, never on its
// silence.
func TestRefusedRowsAreRequeuedOnlyOnAnExplicitNo(t *testing.T) {
	for name, enforced := range map[string]string{
		"field absent":  ``,
		"still pruning": `,"retention_enforced":true`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"server_version":"v1.0.0","events_received":0`+enforced+`}`)
			}))
			defer srv.Close()
			st := storeWith(t, 2)
			if _, err := st.Refused(context.Background(), "event", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			c := New(srv.URL, "t", "v1.0.0", quietLog())
			if _, err := c.Push(context.Background(), st, "m"); err != nil {
				t.Fatal(err)
			}
			if n, _ := st.CountRefused(context.Background()); n != 2 {
				t.Fatalf("refused = %d, want 2 -- rows were re-queued without the server saying so", n)
			}
		})
	}
}

// `enroll` saves on the strength of Check, and what Check sends must give the
// server nothing to store.
func TestCheckPassesOnlyForATrackerThatAcceptsTheToken(t *testing.T) {
	ctx := context.Background()
	var sent map[string]any
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = nil
		_ = json.NewDecoder(r.Body).Decode(&sent)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer right" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid token"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.IngestAck{ServerVersion: "v1.0.0"})
	}))
	defer tracker.Close()

	if err := New(tracker.URL, "right", "v1.0.0", quietLog()).Check(ctx); err != nil {
		t.Fatalf("a tracker accepting the token failed the check: %v", err)
	}
	for k, v := range sent {
		switch k {
		case "v", "agent_version":
		case "machine_id":
			if v != "" {
				t.Errorf("the check named a machine (%v); the server would record it", v)
			}
		default:
			t.Errorf("the check sent %q; it must carry nothing the server stores", k)
		}
	}

	err := New(tracker.URL, "wrong", "v1.0.0", quietLog()).Check(ctx)
	if err == nil || !AuthRejected(err.Error()) {
		t.Errorf("a refused token gave %v, want an error AuthRejected recognises", err)
	}

	for name, h := range notAnAck {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if err := New(srv.URL, "right", "v1.0.0", quietLog()).Check(ctx); err == nil {
				t.Fatal("the check passed against a reply that acknowledged nothing")
			}
		})
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := New(gone.URL, "right", "v1.0.0", quietLog()).Check(ctx); err == nil {
		t.Error("the check passed against a server that is not there")
	}
}

// Telling a machine that upgraded first to upgrade again sends it round in
// circles.
func TestTheStaleAgentWarningFiresOnlyForANewerServer(t *testing.T) {
	for _, c := range []struct {
		agent, server string
		warn          bool
	}{
		{"v1.4.0", "v1.5.0", true},
		{"v1.4.0", "v1.10.0", true},
		{"v1.5.0", "v1.4.0", false},
		{"v1.4.0", "v1.4.0", false},
		{"dev", "v1.4.0", true},
		{"v1.4.0", "6387414-dirty", false},
	} {
		var buf strings.Builder
		cl := New("http://x", "", c.agent, slog.New(slog.NewTextHandler(&buf, nil)))
		cl.noteServerVersion(c.server)
		if got := strings.Contains(buf.String(), "newer release"); got != c.warn {
			t.Errorf("agent %s, server %s: warned = %v, want %v", c.agent, c.server, got, c.warn)
		}
	}
}

// A server that stops pruning still refuses the days its rollups cover but
// takes the rest; waiting for "keeps everything" leaves those rows refused.
func TestRowsAboveALoweredFloorAreOfferedAgain(t *testing.T) {
	st := storeWith(t, 0)
	ctx := context.Background()
	day := func(d string) time.Time { tm, _ := time.Parse(time.DateOnly, d); return tm }
	var recs []store.Record
	for _, d := range []string{"2026-01-10", "2026-03-10"} {
		recs = append(recs, store.Record{ID: d, TS: day(d), TotalTokens: 1, Collector: 1,
			Payload: schema.Event{ID: d, TS: day(d)}})
	}
	if _, err := st.CommitFile(ctx, "", 0, 0, recs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Refused(ctx, "event", day("2026-06-01")); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b schema.Batch
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, _ := gzip.NewReader(r.Body)
			body = zr
		}
		_ = json.NewDecoder(body).Decode(&b)
		_ = json.NewEncoder(w).Encode(schema.IngestAck{ServerVersion: "v1.0.0",
			EventsReceived: len(b.Events), RetentionFloor: "2026-02-01", RetentionEnforced: true})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "t", "v1.0.0", quietLog()).Push(ctx, st, "m"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountRefused(ctx); n != 1 {
		t.Fatalf("refused = %d, want 1: only the row below the new floor stays refused", n)
	}
	if n := unsent(t, st); n != 0 {
		t.Fatalf("%d rows still queued; the re-offered row should have been delivered", n)
	}
}
