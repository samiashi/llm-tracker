package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/db"
)

// fetch serves h over a real connection and returns the status and header
// sent, with the body decoded as that header declares. The transport never
// decompresses, so the header is what is under test.
func fetch(t *testing.T, h http.Handler, header http.Header) (int, http.Header, []byte) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("declared gzip, but the body is not: %v", err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, res.Header, body
}

// text writes a compressible body well past minGzipBytes.
var text = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(strings.Repeat("a", 5000)))
})

// seedModels stores n events under distinct model names, enough to push a
// breakdown or export past minGzipBytes.
func seedModels(t *testing.T, s *Server, n int) {
	t.Helper()
	var many []schema.Event
	for i := range n {
		many = append(many, event(fmt.Sprintf("e%d", i),
			fmt.Sprintf("claude-opus-5-variant-%03d", i), "anthropic:a", 100))
	}
	seed(t, s, many...)
}

func TestResponsesAreCompressedWhenTheClientAsks(t *testing.T) {
	s := newServer(t)
	seedModels(t, s, 60)
	h := WithGzip(s.Routes(http.NotFoundHandler()))

	req := httptest.NewRequest("GET", "/v1/export.csv", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if enc := res.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if !strings.Contains(strings.Join(res.Header.Values("Vary"), ","), "Accept-Encoding") {
		t.Errorf("Vary = %v, want it to include Accept-Encoding", res.Header.Values("Vary"))
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("the body is not valid gzip: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "day,person") {
		t.Fatalf("decompressed body is not the CSV: %.80s", body)
	}
}

func TestResponsesAreNotCompressedUnasked(t *testing.T) {
	s := newServer(t)
	seedModels(t, s, 60)
	h := WithGzip(s.Routes(http.NotFoundHandler()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/export.csv", nil))
	res := rec.Result()
	defer res.Body.Close()
	if enc := res.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want none", enc)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "day,person") {
		t.Fatalf("body is not the CSV: %.80s", body)
	}
}

// writeJSON sets its status before its body, so a decision taken at
// WriteHeader would never compress it.
func TestJSONResponsesAreCompressed(t *testing.T) {
	s := newServer(t)
	seedModels(t, s, 200)
	h := WithGzip(s.Routes(http.NotFoundHandler()))

	req := httptest.NewRequest("GET", "/v1/breakdown?by=model", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Groups []db.Group `json:"groups"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decompressed body is not the breakdown: %v", err)
	}
	if len(out.Groups) == 0 {
		t.Fatal("no groups in the decompressed response")
	}
}

func TestBodylessResponsesStillSendTheirStatus(t *testing.T) {
	h := WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if enc := rec.Result().Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("a 204 was framed as %q", enc)
	}
}

func TestErrorStatusesSurviveTheGzipWrapper(t *testing.T) {
	s := newServer(t)
	h := WithGzip(s.Routes(http.NotFoundHandler()))
	req := httptest.NewRequest("GET", "/v1/breakdown?by=not-a-dimension", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAcceptEncodingIsParsedNotSubstringMatched(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"gzip", true}, {"gzip, deflate", true}, {"deflate, gzip;q=0.9", true},
		{"gzip;q=0", false}, {"identity, gzip;q=0", false},
		{"br, *;q=0, gzip;q=0.0", false}, {"gzip;q=0.000", false},
		{"x-gzip", false}, {"", false}, {"deflate", false},
		{"GZIP", true}, {" gzip ; q=1.0 ", true},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.in); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The middleware honours a refusal, not just the parser.
func TestGzipIsNotSentToAClientThatRefusesIt(t *testing.T) {
	for ae, want := range map[string]string{
		"gzip":                "gzip",
		"gzip;q=0":            "",
		"identity, gzip;q=0":  "",
		"br;q=1, gzip;q=0.00": "",
	} {
		_, hdr, body := fetch(t, WithGzip(text), http.Header{"Accept-Encoding": {ae}})
		if got := hdr.Get("Content-Encoding"); got != want {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want %q", ae, got, want)
		}
		if len(body) != 5000 {
			t.Errorf("Accept-Encoding %q: body decoded to %d bytes, want 5000", ae, len(body))
		}
	}
}

func TestRangeResponsesAreNeverCompressed(t *testing.T) {
	content := strings.Repeat("0123456789", 500)
	h := WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.txt", time.Time{}, strings.NewReader(content))
	}))
	status, hdr, body := fetch(t, h, http.Header{"Accept-Encoding": {"gzip"}, "Range": {"bytes=0-1999"}})
	if status != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", status)
	}
	if enc := hdr.Get("Content-Encoding"); enc != "" {
		t.Fatalf("a range response was framed as %q under Content-Range %q",
			enc, hdr.Get("Content-Range"))
	}
	if string(body) != content[:2000] {
		t.Fatalf("body is not the requested range: %.40q", body)
	}
}

func TestFlushBeforeTheFirstWriteStillDescribesTheBody(t *testing.T) {
	want := strings.Repeat("a", 5000)
	h := WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(want))
	}))
	_, _, body := fetch(t, h, http.Header{"Accept-Encoding": {"gzip"}})
	if string(body) != want {
		t.Fatalf("the body does not match its headers: %.20q", body)
	}
}
