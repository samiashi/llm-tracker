package api

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// The dashboard's response types (dashboard/src/api.ts) are written by hand
// against these handlers. This test serves every endpoint the dashboard calls
// from a seeded database and compares each response's shape -- keys and value
// kinds, not values -- with a committed fixture; dashboard/tests/contract.test.ts
// type-checks the same fixtures against api.ts. A change on either side fails
// one of them until the other is brought along. After a deliberate change:
//
//	go test ./server/internal/api -run DashboardContract -update
var update = flag.Bool("update", false, "rewrite the dashboard's API fixtures from live responses")

const fixtureDir = "../../../dashboard/tests/fixtures/api"

// dashboardEndpoints is every request api.ts makes, with its parameters.
var dashboardEndpoints = map[string]string{
	"summary":     "/v1/summary?from=2026-06-01&to=2026-06-30",
	"daily":       "/v1/daily?from=2026-06-01&to=2026-06-30",
	"daily-model": "/v1/daily/model?from=2026-06-01&to=2026-06-30&top=6",
	"breakdown":   "/v1/breakdown?by=model&from=2026-06-01&to=2026-06-30",
	"heatmap":     "/v1/heatmap?from=2026-06-01&to=2026-06-30",
	"sessions":    "/v1/sessions/top?from=2026-06-01&to=2026-06-30&limit=50",
	"health":      "/v1/health/sources",
	"agents":      "/v1/agents",
	"unknown":     "/v1/unknown",
	"compare":     "/v1/compare?from=2026-06-01&to=2026-06-30",
	"matrix":      "/v1/matrix?rows=model&cols=effort&from=2026-06-01&to=2026-06-30&limit=12",
}

// clockKeys come from the server's clock; pinned when a fixture is written so
// a regeneration does not rewrite them.
var clockKeys = map[string]bool{
	"now": true, "first_seen": true, "last_sync": true, "last_seen": true, "last_event": true,
}

// zoneKeys depend on the server's time zone, which differs between the
// machines that regenerate fixtures; pinned for the same reason, per fixture,
// to what a server on UTC writes for the seeded event.
var zoneKeys = map[string]map[string]any{
	"heatmap": {"utc_offset_minutes": 0.0, "day": "2026-06-15", "weekday": 1.0, "hour": 14.0},
}

func TestDashboardContract(t *testing.T) {
	s := newServer(t)
	s.Version = "v1.4.0"

	// Every array the dashboard reads must come back non-empty: an empty one
	// in a fixture type-checks against anything.
	e := event("contract-1", "claude-opus-5", "anthropic:a", 1_000)
	e.TS = time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)
	e.Effort, e.SessionID, e.ProjectPath = "high", "session-1", "/p"
	if _, err := s.DB.Ingest(context.Background(), &schema.Batch{
		V: schema.Version, MachineID: "m", Hostname: "host", AgentVersion: "v1.4.0",
		Events:   []schema.Event{e},
		Accounts: []schema.Account{{Ref: "anthropic:a", Provider: "anthropic", Email: "dev@example.com"}},
		UnknownSource: []schema.UnknownSource{{
			V: schema.Version, MachineID: "m", Path: "/Users/dev/.amp", Hint: "Amp",
			SizeBytes: 1, Status: schema.StatusTodo,
		}},
		UnknownComplete: true,
	}); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(dashboardEndpoints))
	for n := range dashboardEndpoints {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Routes(http.NotFoundHandler()).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, dashboardEndpoints[name], nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var live any
			if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
				t.Fatal(err)
			}
			if empty := emptyArrays(live, name); len(empty) > 0 {
				t.Fatalf("empty arrays %v: seed something for them, or the fixture "+
					"proves nothing on the dashboard side", empty)
			}

			path := filepath.Join(fixtureDir, name+".json")
			if *update {
				pin(live, zoneKeys[name])
				b, err := json.MarshalIndent(live, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no fixture for %s (run with -update): %v", name, err)
			}
			var committed any
			if err := json.Unmarshal(raw, &committed); err != nil {
				t.Fatal(err)
			}
			if got, want := shape(live), shape(committed); !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(want)
				t.Fatalf("%s no longer has the shape the dashboard was built against.\n"+
					" live:    %s\n fixture: %s\nIf the change is deliberate, run with "+
					"-update and fix dashboard/src/api.ts until `npx tsc -b` passes.",
					dashboardEndpoints[name], g, w)
			}
		})
	}
}

// shape reduces a decoded JSON value to its structure: objects to their keys,
// arrays to their first element, scalars to a kind.
func shape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = shape(e)
		}
		return out
	case []any:
		if len(x) == 0 {
			return []any{}
		}
		return []any{shape(x[0])}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return reflect.TypeOf(v).String()
}

func emptyArrays(v any, at string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			out = append(out, emptyArrays(e, at+"."+k)...)
		}
	case []any:
		if len(x) == 0 {
			return []string{at}
		}
		out = append(out, emptyArrays(x[0], at+"[0]")...)
	}
	return out
}

func pin(v any, zone map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if _, isNum := e.(float64); isNum && clockKeys[k] {
				x[k] = float64(1_781_000_000)
				continue
			}
			if z, ok := zone[k]; ok {
				x[k] = z
				continue
			}
			pin(e, zone)
		}
	case []any:
		for _, e := range x {
			pin(e, zone)
		}
	}
}
