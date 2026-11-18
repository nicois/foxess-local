package web

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// csp is the starter CSP (brief's set): no 'unsafe-inline' (no inline
// handlers or styles), nothing external in connect-src (LAN-only).
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"

// Cache policies: the vendor files are version-pinned (vendor.txt) —
// immutable for a year; everything else revalidates (the UI is
// per-release, not per-asset).
const (
	cacheVendor = "public, max-age=31536000, immutable"
	cacheAssets = "no-cache"
)

// Handler builds the UI handler. It is mounted LAST in buildMux as the
// "/" catch-all — the Go 1.22+ pattern-matched mux makes the specific
// routes (/api/v1, /healthz, the sim prefixes) win over it.
//
//	Surface:  GET / and GET /index.html  →  the SPA shell (no-cache)
//	GET /static/...                   →  the embedded assets
//	anything else                     →  404 (no index fallback, no traversal)
func Handler() http.Handler {
	return http.HandlerFunc(serveUI)
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")

	p := path.Clean(r.URL.Path)
	switch {
	case p == "/" || p == "/index.html":
		serveFile(w, "static/index.html", cacheAssets)
	case strings.HasPrefix(p, "/static/"):
		name := strings.TrimPrefix(p, "/")
		cc := cacheAssets
		if strings.HasPrefix(name, "static/vendor/") {
			cc = cacheVendor
		}
		serveFile(w, name, cc)
	default:
		http.NotFound(w, r)
	}
}

// serveFile writes one embedded asset (404 when missing; the embed FS
// also rejects any traversal, and the mux cleans the path before us).
func serveFile(w http.ResponseWriter, name, cacheControl string) {
	f, err := staticFS.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// contentType gives the assets explicit, stable types: Go's mime table
// says text/javascript for .js; the pin test asserts the IANA
// application/javascript.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
			return ct
		}
		return "application/octet-stream"
	}
}
