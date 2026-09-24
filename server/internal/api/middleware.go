package api

import (
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// WithSecurityHeaders sets the headers a browser needs to be told explicitly.
// The person filter travels in the query string, so without a referrer
// policy that email would leak to any external link the page grows.
//
// Cache-Control: no-store is a default, set before the handler runs so one
// that writes its own headers cannot forget it. Only the static handler
// replaces it, for the files it serves; writeJSON sets it again for /v1.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		// Everything is bundled and self-hosted, so no external origin is
		// needed for scripts, styles or data.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			s.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

// withRecovery turns a handler panic into a logged 500 instead of a stack
// trace on stderr and a connection dropped without a status.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackedWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			s.Log.Error("handler panic", "path", r.URL.Path, "value", v,
				"stack", string(debug.Stack()))
			// Once the response has begun, an error document appended to it
			// reads as part of one complete response. Aborting drops the
			// connection, the only honest signal left.
			if tw.wrote {
				panic(http.ErrAbortHandler)
			}
			writeJSON(tw, http.StatusInternalServerError,
				map[string]string{"error": "internal error"})
		}()
		next.ServeHTTP(tw, r)
	})
}

// trackedWriter records whether the response has begun.
type trackedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackedWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackedWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Flush keeps a streaming handler working through the wrapper.
func (t *trackedWriter) Flush() {
	t.wrote = true
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
