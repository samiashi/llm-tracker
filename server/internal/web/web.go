// Package web serves the dashboard bundle from inside the binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built dashboard. Its committed placeholder lets a clean
// checkout build the server before the frontend is built.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the embedded dashboard.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // unreachable: "dist" is a valid path
	}
	return NewHandler(sub)
}

// NewHandler serves the files in files, and index.html for any other path so
// a client-side route survives a refresh.
//
// Hashed files under /assets never change under their name, so a browser may
// keep them for a year; everything else is revalidated, so a deploy is seen
// at once. These replace WithSecurityHeaders' no-store default, and only on a
// file actually served.
func NewHandler(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		asset := strings.HasPrefix(name, "assets/")
		if info, err := fs.Stat(files, name); err == nil && !info.IsDir() {
			if asset {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// A page from before a deploy asks for bundles the deploy replaced;
		// a 200 of HTML under a script's name hides that they are gone.
		if asset {
			http.NotFound(w, r)
			return
		}
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "dashboard not built (run: make dashboard)", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
