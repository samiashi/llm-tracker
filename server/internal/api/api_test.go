package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/db"
)

// testToken is the ingest token the latest newServer enrolled, so ingest runs
// through the same authorisation path production does. Package tests run one
// at a time, and a stale one fails with a 401 rather than passing wrongly.
var testToken string

func newServer(t *testing.T) *Server {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if testToken, err = d.IssueToken(context.Background(), "tester", "test"); err != nil {
		t.Fatal(err)
	}
	return &Server{DB: d, Log: slog.New(slog.DiscardHandler), Enroll: &fakeGitHub{member: true}}
}

// ingestReq builds an authorised ingest request.
func ingestReq(body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

// seed stores events from machine m, registering accounts anthropic:a and
// anthropic:b for them to resolve to.
func seed(t *testing.T, s *Server, events ...schema.Event) {
	t.Helper()
	if _, err := s.DB.Ingest(context.Background(), "tester", &schema.Batch{
		V: schema.Version, MachineID: "m", Events: events,
		Accounts: []schema.Account{
			{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"},
			{Ref: "anthropic:b", Provider: "anthropic", Email: "other@example.com"},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func event(id, model, ref string, tokens int64) schema.Event {
	return schema.Event{
		V: schema.Version, ID: id, Source: schema.SourceClaudeCode,
		TS: time.Now(), MachineID: "m", AccountRef: ref,
		Model: model, CostBasis: schema.CostRateCard,
		Usage: schema.Usage{InputTokens: tokens},
	}
}

func getCSV(t *testing.T, s *Server, target string) (http.Header, [][]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s: %d %s", target, res.StatusCode, body)
	}
	recs, err := csv.NewReader(res.Body).ReadAll()
	if err != nil {
		t.Fatalf("parsing the download: %v", err)
	}
	return res.Header, recs
}

// A bare request still serves a 30-day window, and a saved CSV carries no
// record of its range but the filename.
func TestExportFilenameNamesTheRangeActuallyServed(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	hdr, _ := getCSV(t, s, "/v1/export.csv")
	got := hdr.Get("Content-Disposition")

	if strings.Contains(got, "range-to-range") {
		t.Fatalf("filename lost the range: %s", got)
	}
	today := time.Now().UTC().Format("2006-01-02")
	from := time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	want := `attachment; filename="llm-tracker-` + from + "-to-" + today + `.csv"`
	if got != want {
		t.Fatalf("Content-Disposition\n got: %s\nwant: %s", got, want)
	}
}

// Exporting the whole team from a page scoped to one person is a privacy
// failure, not just a wrong number.
func TestExportRespectsThePersonFilter(t *testing.T) {
	s := newServer(t)
	seed(t, s,
		event("a", "claude-opus-5", "anthropic:a", 100),
		event("b", "claude-fable-5-1", "anthropic:b", 200),
	)

	_, rows := getCSV(t, s, "/v1/export.csv?person=dev@example.com")
	if len(rows) != 2 {
		t.Fatalf("got %d rows (incl. header), want 2", len(rows))
	}
	if person := rows[1][1]; person != "dev@example.com" {
		t.Fatalf("exported %q under a filter for dev@example.com", person)
	}
}

func TestExportNeutralisesSpreadsheetFormulas(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", `=HYPERLINK("http://evil","click")`, "anthropic:a", 100))

	_, rows := getCSV(t, s, "/v1/export.csv")
	model := rows[1][3]
	if !strings.HasPrefix(model, "'") {
		t.Fatalf("formula reached the file unescaped: %q", model)
	}
}

func TestCSVSafe(t *testing.T) {
	dangerous := []string{"=cmd", "+1", "-1", "@SUM", "\tx", "\rx"}
	for _, v := range dangerous {
		if got := csvSafe(v); !strings.HasPrefix(got, "'") {
			t.Errorf("csvSafe(%q) = %q, want a leading quote", v, got)
		}
	}
	for _, v := range []string{"", "claude-opus-5", "dev@example.com", "2026-09-22"} {
		if got := csvSafe(v); got != v {
			t.Errorf("csvSafe(%q) = %q, want it unchanged", v, got)
		}
	}
}

func TestSafeDatePart(t *testing.T) {
	cases := map[string]string{
		"2026-09-22":            "2026-09-22",
		`x"; rm -rf /; foo="`:   "range",
		"2026-09-22\r\nX: evil": "range",
		"2026-9-1":              "range",
		"":                      "range",
	}
	for in, want := range cases {
		if got := safeDatePart(in); got != want {
			t.Errorf("safeDatePart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentsIgnoreTheDateWindow(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	for _, target := range []string{
		"/v1/agents",
		"/v1/agents?from=1999-01-01&to=1999-01-02",
	} {
		rec := httptest.NewRecorder()
		s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
		var got struct {
			Agents []db.AgentRow `json:"agents"`
			Now    int64         `json:"now"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Agents) != 1 {
			t.Fatalf("GET %s returned %d agents, want 1", target, len(got.Agents))
		}
		if got.Now == 0 {
			t.Fatal("no server clock returned; a skewed browser would misjudge every row")
		}
	}
}

func TestAgentsNameThePersonBehindTheMachine(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	rows, err := s.DB.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d agents, want 1", len(rows))
	}
	if rows[0].Person != "dev@example.com" {
		t.Fatalf("person = %q, want dev@example.com", rows[0].Person)
	}
	if rows[0].LastSync == 0 {
		t.Fatal("last_sync is zero, so every agent would read as never having reported")
	}
}

// See db.AgentRow: the table reports on the collector, not on when a person
// last worked.
func TestAgentRowCarriesNoActivityTimestamp(t *testing.T) {
	banned := []string{"lastevent", "lastactive", "lastseenevent", "lastusage", "lastactivity"}
	rt := reflect.TypeOf(db.AgentRow{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		name := strings.ToLower(f.Name)
		tag := strings.ToLower(f.Tag.Get("json"))
		for _, b := range banned {
			if name == b || strings.HasPrefix(tag, strings.ReplaceAll(b, "last", "last_")) {
				t.Errorf("AgentRow.%s reports when a person last worked; the table is for "+
					"collector health, not attendance", f.Name)
			}
		}
	}
}

// Same target, delivered to the dashboard rather than to the agent.
func TestAgentsReportTheUpgradeTarget(t *testing.T) {
	s := newServer(t)
	s.Version = "v1.4.0"
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents", nil))
	var got struct {
		ServerVersion string `json:"server_version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ServerVersion != "v1.4.0" {
		t.Fatalf("server_version = %q, want v1.4.0", got.ServerVersion)
	}
}

// A working-tree build is no upgrade target, but its version is still the
// honest answer to "what are you running".
func TestServerVersionIsReportedVerbatimEvenWhenNotARelease(t *testing.T) {
	for _, v := range []string{"dev", "6387414-dirty", "v1.4.0"} {
		s := newServer(t)
		s.Version = v
		rec := httptest.NewRecorder()
		s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents", nil))
		var got struct {
			ServerVersion string `json:"server_version"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.ServerVersion != v {
			t.Errorf("server_version = %q, want %q", got.ServerVersion, v)
		}
	}
}

// Reads are unauthenticated on a loopback deployment: whatever a 500 echoes,
// any caller sees.
func TestInternalErrorsSayNothingUseful(t *testing.T) {
	s := newServer(t)
	s.DB.Close() // every query now fails inside the driver

	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/summary", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal error") {
		t.Fatalf("body = %q, want a fixed message", body)
	}
	for _, leak := range []string{"sql", "SQL", "event_daily", "SELECT", ".db", "driver"} {
		if strings.Contains(body, leak) {
			t.Errorf("internal detail %q reached the client: %s", leak, body)
		}
	}
}

// get serves one GET through the routes.
func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func getJSON(t *testing.T, s *Server, target string, v any) {
	t.Helper()
	rec := get(t, s, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
}

func TestWindowedEndpointsRefuseAnImpossibleRange(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	for _, path := range []string{
		"/v1/summary", "/v1/breakdown?by=model", "/v1/daily", "/v1/compare",
		"/v1/matrix?rows=model&cols=source", "/v1/export.csv", "/v1/heatmap",
		"/v1/sessions/top", "/v1/daily/model",
	} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		for _, q := range []string{
			"from=2026-04-01&to=2026-04-31",
			"from=2026-09-01&to=2026-09-3",
			"from=banana&to=2026-12-31",
			"from=2026-09-10&to=2026-09-01",
			"to=2000-01-01",
		} {
			if rec := get(t, s, path+sep+q); rec.Code != http.StatusBadRequest {
				t.Errorf("%s%s%s: status %d, want 400", path, sep, q, rec.Code)
			}
		}
		for _, q := range []string{"from=2026-09-01&to=2026-09-30", "from=2026-09-01&to=2026-09-01", ""} {
			if rec := get(t, s, path+sep+q); rec.Code != http.StatusOK {
				t.Errorf("%s%s%s: status %d, want 200 (%s)", path, sep, q, rec.Code, rec.Body.String())
			}
		}
	}
}

// A 404 sends the caller looking for a typo that is not there.
func TestAWrongMethodIsNotAMissingEndpoint(t *testing.T) {
	s := newServer(t)
	for _, tc := range []struct {
		method, path string
		want         int
		allow        string
	}{
		{"POST", "/v1/summary", http.StatusMethodNotAllowed, "GET, HEAD"},
		{"DELETE", "/v1/export.csv", http.StatusMethodNotAllowed, "GET, HEAD"},
		{"GET", "/v1/ingest", http.StatusMethodNotAllowed, "POST"},
		{"GET", "/v1/no-such-endpoint", http.StatusNotFound, ""},
		{"POST", "/v1/no-such-endpoint", http.StatusNotFound, ""},
	} {
		rec := httptest.NewRecorder()
		s.Routes(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want || rec.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d Allow=%q, want %d Allow=%q",
				tc.method, tc.path, rec.Code, rec.Header().Get("Allow"), tc.want, tc.allow)
		}
	}
}

// Days kept only as rollups must read as "not kept", not as idle.
func TestHeatmapSaysWhereHourlyDetailBegins(t *testing.T) {
	s := newServer(t)
	var fresh map[string]json.RawMessage
	getJSON(t, s, "/v1/heatmap", &fresh)
	if _, ok := fresh["detail_from"]; ok {
		t.Fatalf("detail_from is set on a server that never pruned: %s", fresh["detail_from"])
	}

	old := event("old", "claude-opus-5", "anthropic:a", 100)
	old.TS = time.Now().AddDate(0, 0, -40)
	seed(t, s, old, event("new", "claude-opus-5", "anthropic:a", 100))
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly)
	if _, err := s.DB.Prune(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}

	var got struct {
		DetailFrom string `json:"detail_from"`
	}
	getJSON(t, s, "/v1/heatmap", &got)
	if got.DetailFrom != cutoff {
		t.Fatalf("detail_from = %q, want the retention floor %s", got.DetailFrom, cutoff)
	}

	// With pruning off, agents re-deliver what they held back. The floor
	// stays where it was; the detail moves back with the events.
	resent := event("resent", "claude-opus-5", "anthropic:a", 100)
	resent.TS = time.Now().AddDate(0, 0, -35)
	seed(t, s, resent)
	getJSON(t, s, "/v1/heatmap", &got)
	if want := resent.TS.UTC().Format(time.DateOnly); got.DetailFrom != want {
		t.Fatalf("after a re-delivered day, detail_from = %q, want %s", got.DetailFrom, want)
	}
}
