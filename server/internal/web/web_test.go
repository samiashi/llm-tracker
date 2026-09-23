package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// A built dashboard in miniature; the embedded one is empty in a fresh clone.
var built = fstest.MapFS{
	"index.html":             {Data: []byte("<!doctype html><title>dashboard</title>")},
	"favicon.svg":            {Data: []byte("<svg/>")},
	"assets/index-abc123.js": {Data: []byte("console.log(1)")},
}

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestClientRoutesGetThePage(t *testing.T) {
	h := NewHandler(built)
	for _, path := range []string{"/", "/agents", "/some/client/route"} {
		rec := serve(h, path)
		if rec.Code != http.StatusOK || rec.Body.String() != string(built["index.html"].Data) {
			t.Errorf("%s: %d %.40q, want the page", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAMissingAssetIsNotFound(t *testing.T) {
	h := NewHandler(built)
	for _, path := range []string{"/assets/index-gone.js", "/assets/", "/assets/nested/x.css"} {
		if rec := serve(h, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestOnlyHashedAssetsAreCachedForLong(t *testing.T) {
	h := NewHandler(built)
	for path, want := range map[string]string{
		"/assets/index-abc123.js": "public, max-age=31536000, immutable",
		"/":                       "no-cache",
		"/favicon.svg":            "no-cache",
		"/some/client/route":      "no-cache",
		"/assets/index-gone.js":   "",
	} {
		if got := serve(h, path).Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, want)
		}
	}
}

// A fresh clone embeds no dashboard; the server still starts and says why
// the page is missing.
func TestAnUnbuiltDashboardIsNotFound(t *testing.T) {
	if rec := serve(NewHandler(fstest.MapFS{}), "/"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
