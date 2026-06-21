// Package web embeds the built static UI assets (ui/ -> dist) into the binary
// and serves them with SPA fallback. A placeholder dist/index.html is checked
// in so `go build` succeeds before any real UI build.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded UI assets. Unknown
// paths that are not asset requests fall back to index.html so client-side
// (hash) routing works.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Embedding guarantees "dist" exists at build time; panic surfaces a
		// build-time/packaging error rather than silently serving nothing.
		panic("web: embedded dist directory missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		if _, err := fs.Stat(sub, upath); err != nil {
			// Not a real asset: serve the SPA entrypoint for client routing.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
