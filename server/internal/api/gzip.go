package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// WithGzip compresses responses for clients that accept it. Already-compressed
// types are skipped -- gzipping a PNG spends CPU to make it slightly bigger --
// and so are responses small enough that the framing costs more than it saves.
func WithGzip(next http.Handler) http.Handler {
	pool := &sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		zw, _ := pool.Get().(*gzip.Writer)
		defer pool.Put(zw)
		zw.Reset(w)

		gw := &gzipWriter{ResponseWriter: w, zw: zw}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipWriter holds the status back, and with it the decision to compress,
// until the first write: the decision needs the first chunk, for its size and,
// when nothing set one, its content type.
type gzipWriter struct {
	http.ResponseWriter
	zw      *gzip.Writer
	decided bool
	using   bool
	// wroteHdr records that the handler chose a status; sentHdr, that it has
	// been written out.
	wroteHdr bool
	sentHdr  bool
	status   int
}

// minGzipBytes is roughly where the gzip header and trailer stop being most
// of the payload.
const minGzipBytes = 1024

func (g *gzipWriter) decide(b []byte) {
	if g.decided {
		return
	}
	g.decided = true

	h := g.Header()
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(b)
		h.Set("Content-Type", ct)
	}
	switch {
	case h.Get("Content-Encoding") != "":
		// Something upstream already encoded this.
	case g.status == http.StatusPartialContent || h.Get("Content-Range") != "":
		// A range is a slice of the identity body. Compressed, Content-Range
		// measures one representation and the bytes are another, and a
		// client reassembling ranges rebuilds a corrupt file.
	case len(b) < minGzipBytes && h.Get("Content-Length") == "":
		// Too small to be worth framing. Judged on the first chunk, which for
		// a json.Encoder is the whole value.
	case compressible(ct):
		g.using = true
		h.Del("Content-Length") // no longer the encoded length
		// A client would ask for ranges of the identity body, which is not
		// what it is about to receive.
		h.Del("Accept-Ranges")
		h.Set("Content-Encoding", "gzip")
		// The body varies by encoding, so a shared cache must not serve a
		// gzipped response to a client that did not ask for one.
		addVary(h, "Accept-Encoding")
	}
}

func compressible(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch {
	case strings.HasPrefix(ct, "text/"),
		ct == "application/json",
		ct == "application/javascript",
		ct == "image/svg+xml":
		return true
	}
	return false
}

func addVary(h http.Header, v string) {
	for _, existing := range h.Values("Vary") {
		if strings.EqualFold(existing, v) {
			return
		}
	}
	h.Add("Vary", v)
}

// WriteHeader records the status but does not send it: decided here, the
// encoding would be decided on zero bytes.
func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHdr || g.sentHdr {
		return
	}
	g.wroteHdr = true
	g.status = code
	// A body-less status has nothing to frame.
	if code == http.StatusNoContent || code == http.StatusNotModified {
		g.decided, g.using = true, false
		g.sendHeader()
	}
}

// sendHeader flushes the recorded status to the underlying writer, once.
func (g *gzipWriter) sendHeader() {
	if g.sentHdr {
		return
	}
	g.sentHdr = true
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	// decide before sendHeader: it sets Content-Encoding and clears
	// Content-Length, and neither can be changed once the status is out.
	g.decide(b)
	g.sendHeader()
	if g.using {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	// A handler that set a status and wrote nothing still needs it sent, or
	// net/http answers 200 in its place.
	g.sendHeader()
	if g.using {
		_ = g.zw.Close()
	}
}

// Flush keeps streaming responses working through the wrapper. It sends the
// header, so the encoding is decided first: a later Write could no longer
// declare gzip, only frame it. A nil chunk decides against compressing, the
// right answer for a response whose length is not yet known.
func (g *gzipWriter) Flush() {
	g.decide(nil)
	g.sendHeader()
	if g.using {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// acceptsGzip reports whether the client asked for gzip. The tokens are
// parsed, not searched: `gzip;q=0` means "not acceptable", and `x-gzip` is a
// different token.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, p := range fields[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
				continue
			}
			// q=0 in any spelling means refused. Anything else is a weight
			// we do not otherwise act on.
			if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q <= 0 {
				return false
			}
		}
		return true
	}
	return false
}
