// Package ui serves the embedded web dashboard (built from web/ into dist/).
// The daemon mounts Handler() at "/" next to its /api/v1 routes.
package ui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:generate sh -c "cd ../../web && npm ci --no-audit --no-fund && npm run build"

//go:embed all:dist
var dist embed.FS

// Handler serves the dashboard assets. Unknown paths that look like client
// routes fall back to index.html so deep links survive a reload. Hashed
// assets under assets/ are immutable; index.html is never cached so a kran
// upgrade is picked up on the next load.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// The embed directive guarantees dist exists; this is a build bug.
		panic("ui: embedded dist missing: " + err.Error())
	}
	return handler(sub)
}

func handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if strings.HasPrefix(name, "api/") {
			// API routes belong to the daemon; never answer them with HTML.
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "index.html"
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			if path.Ext(name) != "" {
				// A missing file with an extension is a real 404 (stale
				// asset hash, typo), not a client route.
				http.NotFound(w, r)
				return
			}
			name = "index.html"
			data, err = fs.ReadFile(files, name)
			if err != nil {
				http.Error(w, "dashboard assets are missing from this build; run make ui", http.StatusInternalServerError)
				return
			}
		}
		h := w.Header()
		h.Set("Content-Type", contentType(name))
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		if name == "index.html" {
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	})
}

// contentType avoids depending on the host's mime.types (minimal containers
// and macOS differ) for the handful of types the build emits.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".json", ".map":
		return "application/json"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
