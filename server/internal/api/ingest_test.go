package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/db"
)

// postIngest sends body through the routes as an authorised agent would,
// gzipped when gz is set.
func postIngest(t *testing.T, s *Server, body []byte, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		body = buf.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
	return rec
}

// ack decodes a successful ingest reply.
func ack(t *testing.T, rec *httptest.ResponseRecorder) schema.IngestAck {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	var a schema.IngestAck
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func batchOf(t *testing.T, events ...schema.Event) []byte {
	t.Helper()
	b, err := json.Marshal(schema.Batch{V: schema.Version, MachineID: "m", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The version is how an agent learns it is stale without reaching GitHub.
func TestIngestReportsTheServerVersion(t *testing.T) {
	s := newServer(t)
	s.Version = "v1.4.0"

	body := `{"v":1,"machine_id":"m","hostname":"h","agent_version":"v1.3.0","events":[]}`
	if got := ack(t, postIngest(t, s, []byte(body), false)).ServerVersion; got != "v1.4.0" {
		t.Fatalf("server_version = %q, want v1.4.0", got)
	}
}

func TestIngestRejectsEveryCredentialButTheRealOne(t *testing.T) {
	const body = `{"v":1,"machine_id":"m","events":[]}`
	s := newServer(t)
	token := testToken

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"prefix of the token", "Bearer " + token[:8], http.StatusUnauthorized},
		{"the token and more", "Bearer " + token + "x", http.StatusUnauthorized},
		{"missing scheme", token, http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"enrolled-looking, never issued", "Bearer " + schema.EnrolledTokenPrefix + "x", http.StatusUnauthorized},
		{"correct token", "Bearer " + token, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/ingest", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestIngestRequiresJSONContentType(t *testing.T) {
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x"} {
		t.Run(ct, func(t *testing.T) {
			s := newServer(t)
			req := ingestReq(`{"v":1,"machine_id":"m","events":[]}`)
			req.Header.Del("Content-Type")
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q: status = %d, want 415", ct, rec.Code)
			}
		})
	}
}

// Agents compress their uploads; failing to decode that stops every agent.
func TestIngestAcceptsAGzippedBody(t *testing.T) {
	s := newServer(t)
	ack(t, postIngest(t, s, []byte(`{"v":1,"machine_id":"m","hostname":"h","events":[]}`), true))
}

func TestIngestRejectsAMalformedGzipBody(t *testing.T) {
	s := newServer(t)
	req := ingestReq("not gzip at all")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The agent reads received + rejected as the count it sent.
func TestIngestStoresOnlyWhatSanitiseKept(t *testing.T) {
	s := newServer(t)
	good := event("good", "claude-opus-5", "anthropic:a", 100)
	negative := event("negative", "claude-opus-5", "anthropic:a", 0)
	negative.Usage.InputTokens = -1_000_000
	future := event("future", "claude-opus-5", "anthropic:a", 500)
	future.TS = time.Now().AddDate(0, 1, 0)

	a := ack(t, postIngest(t, s, batchOf(t, good, negative, future), false))
	if a.EventsReceived != 1 || a.EventsRejected != 2 {
		t.Fatalf("events_received=%d events_rejected=%d, want 1 and 2 for the 3 sent",
			a.EventsReceived, a.EventsRejected)
	}

	today := time.Now().UTC()
	var sum struct {
		Totals db.Totals `json:"totals"`
	}
	getJSON(t, s, fmt.Sprintf("/v1/summary?from=%s&to=%s",
		today.AddDate(0, 0, -29).Format(time.DateOnly),
		today.AddDate(0, 2, 0).Format(time.DateOnly)), &sum)
	if sum.Totals.Events != 1 || sum.Totals.InputTokens != 100 {
		t.Fatalf("stored %d events totalling %d input tokens; want only the good one (1, 100)",
			sum.Totals.Events, sum.Totals.InputTokens)
	}
}

// One colleague's half-written adapter must not block everyone else's
// backlog, so implausible events are dropped and the rest of the batch lands.
func TestIngestDropsImplausibleEventsAndKeepsTheRest(t *testing.T) {
	future := time.Now().AddDate(0, 0, 30)
	huge := 2.0 * maxNativeCostUSD

	good := event("good", "claude-opus-5", "anthropic:a", 100)
	for _, tc := range []struct {
		name string
		bad  schema.Event
	}{
		{"negative input", func() schema.Event {
			e := event("x", "m", "anthropic:a", 0)
			e.Usage.InputTokens = -1
			return e
		}()},
		{"negative reasoning", func() schema.Event {
			e := event("x", "m", "anthropic:a", 10)
			e.Usage.ReasoningTokens = -5
			return e
		}()},
		{"absurd total", func() schema.Event {
			e := event("x", "m", "anthropic:a", 0)
			e.Usage.InputTokens = maxTokens + 1
			return e
		}()},
		{"far-future timestamp", func() schema.Event {
			e := event("x", "m", "anthropic:a", 10)
			e.TS = future
			return e
		}()},
		{"far-past timestamp", func() schema.Event {
			e := event("x", "m", "anthropic:a", 10)
			e.TS = time.Now().AddDate(-20, 0, 0)
			return e
		}()},
		{"absurd native cost", func() schema.Event {
			e := event("x", "m", "anthropic:a", 10)
			e.NativeCostUSD = &huge
			return e
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, rejected := sanitise([]schema.Event{good, tc.bad}, "m")
			if rejected != 1 {
				t.Fatalf("rejected = %d, want 1", rejected)
			}
			if len(kept) != 1 || kept[0].ID != "good" {
				t.Fatalf("kept = %+v, want just the good event", kept)
			}
		})
	}
}

// A zero timestamp is replaced server-side, so it is not "before the floor".
func TestSanitiseKeepsAZeroTimestamp(t *testing.T) {
	e := event("z", "claude-opus-5", "anthropic:a", 10)
	e.TS = time.Time{}
	kept, rejected := sanitise([]schema.Event{e}, "m")
	if rejected != 0 || len(kept) != 1 {
		t.Fatalf("a zero timestamp was rejected: kept=%d rejected=%d", len(kept), rejected)
	}
}

// A row moves only for the machine that stored it, so a Continue key naming
// another machine, or an id its key does not derive, stored first from here
// would keep that colleague's own readings out.
func TestAContinueKeyMustBeTheUploadersOwn(t *testing.T) {
	keyed := func(native, idFrom string, collector int) schema.Event {
		e := event(schema.MakeID(schema.SourceContinue, idFrom), "gpt-5", "", 10)
		e.Source, e.NativeID, e.Collector = schema.SourceContinue, native, collector
		return e
	}
	const own, alices = "m:0.2.0/tokensGenerated.jsonl#0", "alice:0.2.0/tokensGenerated.jsonl#0"
	for _, tc := range []struct {
		name string
		e    schema.Event
		want int // rejected
	}{
		{"keyed on the uploading machine", keyed(own, own, 10), 0},
		{"keyed on another machine", keyed(alices, alices, 10), 1},
		{"an id its key does not derive", keyed(own, alices, 10), 1},
		{"an older collector's key, which names no machine",
			keyed("0.2.0/tokensGenerated.jsonl#0", "0.2.0/tokensGenerated.jsonl#0", 9), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, rejected := sanitise([]schema.Event{tc.e}, "m"); rejected != tc.want {
				t.Fatalf("rejected = %d, want %d", rejected, tc.want)
			}
		})
	}
}

// Walks schema.Usage, so a counter added later is covered.
func TestEveryCounterIsBoundedBeforeItIsSummed(t *testing.T) {
	ut := reflect.TypeFor[schema.Usage]()
	for i := range ut.NumField() {
		for _, tc := range []struct {
			v    int64
			want int // rejected
		}{{-1, 1}, {maxTokens, 0}, {maxTokens + 1, 1}, {math.MaxInt64, 1}} {
			e := event("x", "claude-opus-5", "anthropic:a", 0)
			reflect.ValueOf(&e.Usage).Elem().Field(i).SetInt(tc.v)
			if _, rejected := sanitise([]schema.Event{e}, "m"); rejected != tc.want {
				t.Errorf("%s = %d: rejected = %d, want %d", ut.Field(i).Name, tc.v, rejected, tc.want)
			}
		}
	}

	e := event("wraps", "claude-opus-5", "anthropic:a", 0)
	e.Usage.InputTokens, e.Usage.OutputTokens = 1<<62, 1<<62
	if _, rejected := sanitise([]schema.Event{e}, "m"); rejected != 1 {
		t.Errorf("input and output of 2^62 were accepted; their total is %d", e.Usage.TotalTokens())
	}
}

// End to end: an overflowing event must not reach a sum any endpoint reads.
func TestIngestRejectsCountsWhoseSumWouldOverflow(t *testing.T) {
	s := newServer(t)
	good := event("good", "claude-opus-5", "anthropic:a", 100)
	wraps := event("wraps", "claude-opus-5", "anthropic:a", 0)
	wraps.Usage.InputTokens, wraps.Usage.OutputTokens = 1<<62, 1<<62
	huge := event("huge", "claude-opus-5", "anthropic:a", 10)
	huge.Usage.ReasoningTokens = math.MaxInt64
	other := event("other", "claude-opus-5", "anthropic:a", 10)
	other.Usage.ReasoningTokens = 1

	a := ack(t, postIngest(t, s, batchOf(t, good, wraps, huge, other), false))
	if a.EventsRejected != 2 {
		t.Errorf("events_rejected = %d, want 2", a.EventsRejected)
	}
	rec := get(t, s, "/v1/summary")
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/summary: %d %s", rec.Code, rec.Body.String())
	}
	var sum struct {
		Totals db.Totals `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Totals.TotalTokens != 110 || sum.Totals.ReasoningTokens != 1 {
		t.Fatalf("total_tokens=%d reasoning_tokens=%d, want 110 and 1",
			sum.Totals.TotalTokens, sum.Totals.ReasoningTokens)
	}
}

// The lists are read off schema.Batch, so one added later is covered: each
// must be shadowed in ingestBody by a bounded list that takes what an agent
// sends and refuses one element past its limit, saying which limit.
func TestEveryListInABatchIsBounded(t *testing.T) {
	// The most an agent sends in one batch: its batch of events, the
	// harnesses scan.go can report, the logins identity reads.
	agentSends := map[string]int{"events": 2_000, "unknown_sources": 17, "accounts": 2}
	shadows := map[string]reflect.Type{}
	for f := range reflect.TypeFor[ingestBody]().Fields() {
		shadows[jsonName(f)] = f.Type
	}
	for f := range reflect.TypeFor[schema.Batch]().Fields() {
		name := jsonName(f)
		if f.Type.Kind() != reflect.Slice {
			continue
		}
		t.Run(name, func(t *testing.T) {
			b, ok := reflect.Zero(shadows[name]).Interface().(interface{ limit() (string, int) })
			if !ok {
				t.Fatalf("%q is not shadowed by a bounded list in ingestBody", name)
			}
			noun, limit := b.limit()
			if limit < agentSends[name] || limit > 10*agentSends[name] {
				t.Fatalf("%q admits %d; an agent sends up to %d, and the limit belongs near that",
					name, limit, agentSends[name])
			}
			list := func(n int) []byte {
				return []byte(`{"v":1,"machine_id":"m","` + name + `":[` +
					strings.TrimSuffix(strings.Repeat("null,", n), ",") + `]}`)
			}
			if rec := postIngest(t, newServer(t), list(limit), true); rec.Code != http.StatusOK {
				t.Fatalf("%d %s: status %d, want 200 (%s)", limit, noun, rec.Code, rec.Body)
			}
			rec := postIngest(t, newServer(t), list(limit+1), true)
			if rec.Code != http.StatusRequestEntityTooLarge ||
				!strings.Contains(rec.Body.String(), fmt.Sprintf("at most %d %s", limit, noun)) {
				t.Fatalf("%d %s: status %d %s, want 413 naming the limit", limit+1, noun, rec.Code, rec.Body)
			}
		})
	}
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// Each batch can hold the byte limit of JSON while it is decoded, so only a
// few are taken at once; the next is told to come back, and its agent keeps
// it until then.
func TestIngestTakesOnlyAFewBatchesAtOnce(t *testing.T) {
	s := newServer(t)
	for range cap(ingestSlots) {
		ingestSlots <- struct{}{}
	}
	rec := postIngest(t, s, batchOf(t), false)
	for range cap(ingestSlots) {
		<-ingestSlots
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("with every slot taken: %d, Retry-After %q; want 503 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	ack(t, postIngest(t, s, batchOf(t), false))
}

// Agents installed before quota readings were dropped still send them, and
// must keep uploading: the list is ignored, and the ack says exactly what it
// would without it.
func TestAnOlderAgentsQuotaReadingsChangeNothing(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal(batchOf(t, event("e", "claude-opus-5", "anthropic:a", 100)), &body); err != nil {
		t.Fatal(err)
	}
	send := func(s *Server) schema.IngestAck {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return ack(t, postIngest(t, s, b, false))
	}
	without := send(newServer(t))

	body["quota"] = []map[string]any{{"id": "q", "source": "codex", "machine_id": "m",
		"account_ref": "openai:a", "used_percent": 91, "window_minutes": 300}}
	s := newServer(t)
	if with := send(s); with != without {
		t.Fatalf("ack %+v with a quota list, %+v without", with, without)
	}
	var sum struct {
		Totals db.Totals `json:"totals"`
	}
	getJSON(t, s, "/v1/summary", &sum)
	if sum.Totals.Events != 1 || sum.Totals.InputTokens != 100 {
		t.Fatalf("stored %d events, %d input tokens; want the batch's one event of 100",
			sum.Totals.Events, sum.Totals.InputTokens)
	}
}

// Past the limit is 413, not a parse error, and whitespace after a complete
// document is not an overrun.
func TestIngestByteLimitAppliesAfterDecompression(t *testing.T) {
	doc := func(size int, trailing string) []byte {
		pre, post := `{"v":1,"machine_id":"m","pad":"`, `","events":[]}`
		return []byte(pre + strings.Repeat("x", size-len(pre)-len(post)) + post + trailing)
	}
	for _, tc := range []struct {
		name string
		body func() []byte
		gz   bool
		want int
	}{
		{"plain, past the limit", func() []byte { return doc(maxIngestBytes+1, "") }, false, http.StatusRequestEntityTooLarge},
		// An agent's batch is under 2 MiB, and each one in flight is held whole.
		{"plain, 17 MiB", func() []byte { return doc(17<<20, "") }, false, http.StatusRequestEntityTooLarge},
		{"gzip, decompresses past the limit", func() []byte { return doc(maxIngestBytes+1, "") }, true, http.StatusRequestEntityTooLarge},
		{"gzip, exactly at the limit", func() []byte { return doc(maxIngestBytes, "") }, true, http.StatusOK},
		{"gzip, trailing whitespace", func() []byte { return doc(1000, strings.Repeat("\n", 64<<10)) }, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postIngest(t, newServer(t), tc.body(), tc.gz)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
			if tc.want == http.StatusRequestEntityTooLarge &&
				!strings.Contains(rec.Body.String(), fmt.Sprintf("at most %d MiB", maxIngestBytes>>20)) {
				t.Fatalf("the 413 does not name the limit: %s", rec.Body)
			}
		})
	}
}

// Strings are clipped wherever they arrive, not only on events: a hostname
// stored whole is sent back on every dashboard poll.
func TestBatchStringsAreClippedBeforeTheyAreServed(t *testing.T) {
	s := newServer(t)
	long := strings.Repeat("x", 1<<20)
	e := event("e", "claude-opus-5", "anthropic:a", 10)
	e.Source = schema.Source(long)
	e.Speed = long
	body, err := json.Marshal(schema.Batch{
		V: schema.Version, MachineID: "m" + long, Hostname: long, AgentVersion: long,
		Events: []schema.Event{e},
		UnknownSource: []schema.UnknownSource{{
			MachineID: "m" + long, Path: long, Hint: long, Note: long,
			Status: schema.UnknownStatus(long),
		}},
		UnknownComplete: true,
		Accounts:        []schema.Account{{Ref: "anthropic:a", Email: long, Provider: long, PlanType: long}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack(t, postIngest(t, s, body, true))

	for _, target := range []string{"/v1/agents", "/v1/unknown", "/v1/breakdown?by=source"} {
		rec := get(t, s, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		if n := rec.Body.Len(); n > 16<<10 {
			t.Errorf("%s returned %d bytes: a string was stored unclipped", target, n)
		}
	}
}

// Walks the wire types, so a string added to them later is covered.
func TestEveryStringOnTheWireIsClipped(t *testing.T) {
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			v.SetString(strings.Repeat("é", maxFieldLen+1))
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					fill(v.Field(i))
				}
			}
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			fill(v.Index(0))
		}
	}
	var in ingestBody
	fill(reflect.ValueOf(&in).Elem())
	b, _ := in.batch()

	var check func(v reflect.Value, path string)
	check = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.String:
			if n := utf8.RuneCountInString(v.String()); n > maxFieldLen {
				t.Errorf("%s is %d runes, want <= %d", path, n, maxFieldLen)
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if f := v.Type().Field(i); f.IsExported() {
					check(v.Field(i), path+"."+f.Name)
				}
			}
		case reflect.Slice:
			if v.Len() == 0 {
				t.Errorf("%s is empty: the batch dropped a list it was sent", path)
			}
			for i := range v.Len() {
				check(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	check(reflect.ValueOf(b), "Batch")
}

func TestClipCountsRunesNotBytes(t *testing.T) {
	got := clip(strings.Repeat("é", maxFieldLen+50), maxFieldLen)
	if n := len([]rune(got)); n != maxFieldLen {
		t.Fatalf("clipped to %d runes, want %d", n, maxFieldLen)
	}
	if !utf8.ValidString(got) {
		t.Fatal("clip produced invalid UTF-8")
	}
}
