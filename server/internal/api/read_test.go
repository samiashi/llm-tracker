package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
