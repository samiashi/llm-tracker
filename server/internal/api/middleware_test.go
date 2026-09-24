package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// main's wiring is covered in package main.
func TestNoAPIResponseIsCacheable(t *testing.T) {
	s := newServer(t)
	seed(t, s, event("a", "claude-opus-5", "anthropic:a", 100))

	for _, tc := range []struct {
		h    http.Handler
		path string
		want int
	}{
		{s.Routes(http.NotFoundHandler()), "/v1/summary", http.StatusOK},
		{WithSecurityHeaders(s.Routes(http.NotFoundHandler())), "/v1/summary", http.StatusOK},
		{WithSecurityHeaders(s.Routes(http.NotFoundHandler())), "/v1/export.csv?person=dev@example.com", http.StatusOK},
		{WithSecurityHeaders(s.Routes(http.NotFoundHandler())), "/healthz", http.StatusOK},
		{WithSecurityHeaders(s.Routes(http.NotFoundHandler())), "/v1/breakdown?by=nope", http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		tc.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.path, rec.Code, tc.want)
		}
		h := rec.Result().Header
		if got := h.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.path, got)
		}
	}
}

// A panic before anything was written becomes a 500 that says nothing.
func TestAPanickingHandlerReturns500(t *testing.T) {
	s := newServer(t)
	h := s.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/summary", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("the panic value reached the client")
	}
}

func TestAPanicAfterTheResponseBeganCutsTheConnection(t *testing.T) {
	s := newServer(t)
	srv := httptest.NewServer(s.withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"groups":[`))
		w.(http.Flusher).Flush()
		panic("boom")
	})))
	defer srv.Close()

	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		return // cut before the header arrived, which is just as honest
	}
	defer res.Body.Close()
	if body, err := io.ReadAll(res.Body); err == nil {
		t.Fatalf("a response cut short by a panic arrived complete: %d %q", res.StatusCode, body)
	}
}

func TestSecurityHeadersCoverWhateverTheyWrap(t *testing.T) {
	h := WithSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Result().Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := rec.Result().Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors: %q", csp)
	}
}
