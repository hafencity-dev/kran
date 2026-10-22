package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><title>kran</title>")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
		"assets/font-x.woff2":     {Data: []byte("wOF2")},
		"favicon.svg":             {Data: []byte("<svg/>")},
	}
	h := handler(files)
	tests := []struct {
		method, path string
		code         int
		ctype, cache string
		body         string
	}{
		{"GET", "/", 200, "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"GET", "/index.html", 200, "text/html; charset=utf-8", "no-cache", "<!doctype"},
		{"GET", "/assets/index-abc123.js", 200, "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", "console"},
		{"GET", "/assets/index-abc123.css", 200, "text/css; charset=utf-8", "public, max-age=31536000, immutable", "body"},
		{"GET", "/assets/font-x.woff2", 200, "font/woff2", "public, max-age=31536000, immutable", "wOF2"},
		{"GET", "/favicon.svg", 200, "image/svg+xml", "no-cache", "<svg"},
		{"GET", "/jobs/01abc", 200, "text/html; charset=utf-8", "no-cache", "<!doctype"}, // SPA fallback
		{"GET", "/assets/index-old.js", 404, "", "", ""},
		{"GET", "/api/v1/nope", 404, "", "", ""},
		{"GET", "/../../etc/passwd", 200, "text/html; charset=utf-8", "no-cache", "<!doctype"}, // cleaned, never leaves dist
		{"HEAD", "/", 200, "text/html; charset=utf-8", "no-cache", ""},
		{"POST", "/", 405, "", "", ""},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.code {
			t.Errorf("%s %s: code %d, want %d", tt.method, tt.path, rec.Code, tt.code)
			continue
		}
		if tt.ctype != "" && rec.Header().Get("Content-Type") != tt.ctype {
			t.Errorf("%s %s: content-type %q", tt.method, tt.path, rec.Header().Get("Content-Type"))
		}
		if tt.cache != "" && rec.Header().Get("Cache-Control") != tt.cache {
			t.Errorf("%s %s: cache-control %q", tt.method, tt.path, rec.Header().Get("Cache-Control"))
		}
		if !strings.HasPrefix(rec.Body.String(), tt.body) {
			t.Errorf("%s %s: body %q", tt.method, tt.path, rec.Body.String())
		}
	}
}

// The committed build must be embedded: go build without Node still ships
// a working dashboard.
func TestEmbeddedBuild(t *testing.T) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("dist/index.html missing: run make ui: %v", err)
	}
	if !strings.Contains(string(idx), "/assets/") {
		t.Fatal("dist/index.html does not reference built assets; run make ui")
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("index: %d, csp %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	for _, d := range []string{"object-src 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), d) {
			t.Errorf("csp lacks %s", d)
		}
	}
}
