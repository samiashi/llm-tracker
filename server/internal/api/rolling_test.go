package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRollingRangeValidationAndRetention(t *testing.T) {
	s := newServer(t)
	routes := s.Routes(http.NotFoundHandler())
	end := time.Now().UTC().Truncate(time.Second).Unix()
	query := fmt.Sprintf("start=%d&end=%d", end-86400, end)
	endpoints := []string{"summary", "daily", "daily/model", "breakdown", "matrix?rows=model&cols=effort", "compare", "export.csv", "heatmap", "sessions/top"}
	request := func(path, q string, want int) {
		t.Helper()
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/"+path+sep+q, nil))
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", path, q, rec.Code, rec.Body.String(), want)
		}
	}
	for _, path := range endpoints {
		request(path, query, 200)
		for _, bad := range []string{"start=oops&end=1", "start=1", "end=1", "start=2&end=1", "start=-1&end=86399", "start=1&end=86402", "start=9223372036854775808&end=1"} {
			request(path, bad, 400)
		}
	}
	// Roll yesterday up. The 24h interval overlaps it even when today has no
	// events, and must not silently turn its missing portion into zero.
	e := event("old", "claude-sonnet-4-6", "anthropic:a", 100)
	e.TS = time.Unix(end-86400, 0)
	seed(t, s, e)
	if _, err := s.DB.Prune(context.Background(), time.Now().UTC().Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	for _, path := range endpoints {
		request(path, query, 422)
	}
	request("summary", "", 200)
}

func TestViewerTimezoneIsValidatedAndReturned(t *testing.T) {
	s := newServer(t)
	routes := s.Routes(http.NotFoundHandler())
	for _, zone := range []string{"Unknown/Nowhere", "Local", "../etc/passwd", strings.Repeat("x", 129)} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/heatmap?timezone="+zone, nil))
		if rec.Code != 400 {
			t.Errorf("zone %q: %d", zone, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/heatmap?timezone=Asia/Dubai", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"timezone":"Asia/Dubai"`) {
		t.Fatalf("viewer zone missing: %d %s", rec.Code, rec.Body.String())
	}
}
