package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// A 400 that names neither the value nor the choices sends the caller to the
// source to find out what the endpoint takes.
func TestABadDimensionNamesItselfAndTheChoices(t *testing.T) {
	s := newServer(t)
	for target, want := range map[string][]string{
		"/v1/breakdown?by=project":            {`"project"`, "by", "model, origin, person, source, surface"},
		"/v1/matrix?rows=project&cols=effort": {`"project"`, "rows", "effort, model, source, speed, surface"},
		"/v1/matrix?rows=model&cols=nonsense": {`"nonsense"`, "cols", "effort, model, source, speed, surface"},
	} {
		rec := get(t, s, target)
		var body struct {
			Error string `json:"error"`
		}
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("GET %s: %d %s, want a 400 saying why", target, rec.Code, rec.Body.String())
		}
		for _, w := range want {
			if !strings.Contains(body.Error, w) {
				t.Errorf("GET %s: %q does not say %s", target, body.Error, w)
			}
		}
	}
}

// The dashboard compares releases and knows no version grammar of its own, so
// the server says which versions are releases: "X.Y.Z", or "" for a build
// from a working tree, which is no upgrade target and nothing to be behind.
func TestOnlyAReleaseIsReportedAsOne(t *testing.T) {
	for version, want := range map[string]string{
		"v1.4.0": "1.4.0", "1.4.0": "1.4.0", "v1.4": "", "v1.4.0-rc1": "",
		"v1.4.0-3-g6387414-dirty": "", "6387414-dirty": "", "dev": "", "": "",
	} {
		t.Run(fmt.Sprintf("%q", version), func(t *testing.T) {
			s := newServer(t)
			s.Version = version
			if _, err := s.DB.Ingest(context.Background(), &schema.Batch{
				V: schema.Version, MachineID: "m", AgentVersion: version,
			}); err != nil {
				t.Fatal(err)
			}
			var agents struct {
				Agents []struct {
					Release string `json:"release"`
				} `json:"agents"`
			}
			getJSON(t, s, "/v1/agents", &agents)
			var summary struct {
				ServerRelease *string `json:"server_release"`
			}
			getJSON(t, s, "/v1/summary", &summary)
			if len(agents.Agents) != 1 || agents.Agents[0].Release != want {
				t.Errorf("agent on %q reported as release %+v, want %q", version, agents.Agents, want)
			}
			if summary.ServerRelease == nil || *summary.ServerRelease != want {
				t.Errorf("server on %q reported as release %v, want %q", version, summary.ServerRelease, want)
			}
		})
	}
}

// Filtered to a person, the history the summary reports is theirs: the
// dashboard calls a prior period before it unrecorded rather than a drop.
func TestTheSummaryHistoryIsThePersonsOwn(t *testing.T) {
	s := newServer(t)
	day := func(daysAgo int) time.Time { return time.Now().AddDate(0, 0, -daysAgo) }
	early := event("early", "claude-opus-5", "anthropic:b", 100)
	early.TS = day(60)
	late := event("late", "claude-opus-5", "anthropic:a", 100)
	late.TS = day(20)
	seed(t, s, early, late)

	var got struct {
		First string `json:"history_first_day"`
	}
	getJSON(t, s, "/v1/summary?person=dev@example.com", &got)
	if want := day(20).UTC().Format(time.DateOnly); got.First != want {
		t.Fatalf("history_first_day = %q for dev@example.com, want their first day %s", got.First, want)
	}
	getJSON(t, s, "/v1/summary", &got)
	if want := day(60).UTC().Format(time.DateOnly); got.First != want {
		t.Fatalf("history_first_day = %q for the team, want %s", got.First, want)
	}
}

// An absent length is the db's default, not one the handler keeps apart.
func TestAnAbsentListLengthIsTheDefault(t *testing.T) {
	s := newServer(t)
	var evs []schema.Event
	for i := range 12 {
		e := event(fmt.Sprintf("e%d", i), fmt.Sprintf("model-%02d", i), "anthropic:a", int64(100+i))
		e.SessionID = fmt.Sprintf("s%d", i)
		evs = append(evs, e)
	}
	seed(t, s, evs...)

	var points struct {
		Points []struct {
			Model string `json:"model"`
		} `json:"points"`
	}
	getJSON(t, s, "/v1/daily/model", &points)
	var sessions struct {
		Sessions []struct{} `json:"sessions"`
	}
	getJSON(t, s, "/v1/sessions/top", &sessions)
	var matrix struct {
		Cells []struct{} `json:"cells"`
	}
	getJSON(t, s, "/v1/matrix?rows=model&cols=effort", &matrix)
	if len(points.Points) != 6 || len(sessions.Sessions) != 10 || len(matrix.Cells) != 6 {
		t.Fatalf("got %d models, %d sessions, %d matrix rows; want the defaults 6, 10 and 6",
			len(points.Points), len(sessions.Sessions), len(matrix.Cells))
	}
}
