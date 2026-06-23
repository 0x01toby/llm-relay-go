// Package web serves the built React console frontend (static assets + SPA
// fallback). It is the Go port of the static-serving logic in console-ui.ts.
//
// The frontend source lives in ../../web/dashboard/ (sibling of the Go
// project). Build it with `cd web/dashboard && npm run build`; the output
// lands in ../../web/dashboard/dist/, which is what this package embeds.
//
// In production the frontend is embedded into the binary via //go:embed so
// the single binary is fully self-contained. When the dist directory is
// absent (e.g. dev mode), the handler serves a "frontend not built" page.
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var frontendFS embed.FS

// mimeTypes mirrors the MIME_TYPES map in console-ui.ts.
var mimeTypes = map[string]string{
	".css":    "text/css; charset=utf-8",
	".html":   "text/html; charset=utf-8",
	".ico":    "image/x-icon",
	".js":     "text/javascript; charset=utf-8",
	".json":   "application/json; charset=utf-8",
	".map":    "application/json; charset=utf-8",
	".png":    "image/png",
	".svg":    "image/svg+xml; charset=utf-8",
	".txt":    "text/plain; charset=utf-8",
	".woff":   "font/woff",
	".woff2":  "font/woff2",
}

// Handler serves the SPA. GET/HEAD only; other methods fall through (return
// 404). Paths with no extension get index.html (SPA routing); paths with an
// extension serve the matching static file.
func Handler() http.Handler {
	sub, err := fs.Sub(frontendFS, "dist")
	if err != nil {
		// Should never happen with a valid embed directive.
		return http.HandlerFunc(missingBuildHandler)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// Bypass API and provider paths (those are handled by other mux entries).
		if strings.HasPrefix(r.URL.Path, "/__console") || strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}

		clean := path.Clean(r.URL.Path)
		ext := path.Ext(clean)

		if ext == "" {
			// SPA route → serve index.html.
			serveIndex(w, r, sub)
			return
		}

		// Static asset: set MIME + cache headers.
		if mime, ok := mimeTypes[ext]; ok {
			w.Header().Set("Content-Type", mime)
		}
		if ext == ".html" {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// dummyModTime is a fixed timestamp for embedded content (embed has no mtime).
var dummyModTime = time.Now()

func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	index, err := fsys.Open("index.html")
	if err != nil {
		missingBuildHandler(w, r)
		return
	}
	defer index.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, index)
}

// missingBuildHandler returns a page explaining the frontend hasn't been built.
func missingBuildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Frontend not built</title>` +
		`<style>body{font-family:system-ui;background:#0d1117;color:#c9d1d9;padding:2rem;text-align:center}` +
		`code{background:#161b22;padding:.2em .4em;border-radius:4px}</style></head><body>` +
		`<h1>Frontend not built</h1><p>Run <code>npm run build</code> in <code>console/ai-proxy-dashboard</code> ` +
		`and copy the output to <code>web/dashboard/dist/</code>.</p></body></html>`))
}
