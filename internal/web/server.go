package web

import (
	"mime"
	"net/http"
	"path"
	"strconv"
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
		// the shell follows the request's Accept-Language (data-lang-hint
		// on <html>): vary it so a shared cache never hands one visitor's
		// variant to another
		a := loadAsset("static/index.html")
		if hint := langHint(r.Header.Get("Accept-Language")); hint != "" {
			if va := loadHintShell(hint); va != nil {
				a = va
			}
		}
		writeAsset(w, r, a, cacheAssets, "Accept-Language")
	case strings.HasPrefix(p, "/static/"):
		name := strings.TrimPrefix(p, "/")
		cc := cacheAssets
		if strings.HasPrefix(name, "static/vendor/") {
			cc = cacheVendor
		}
		serveFile(w, r, name, cc)
	default:
		http.NotFound(w, r)
	}
}

// serveFile writes one embedded asset (404 when missing; the embed FS
// also rejects any traversal, and the mux cleans the path before us).
func serveFile(w http.ResponseWriter, r *http.Request, name, cacheControl string) {
	a := loadAsset(name)
	if a == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeAsset(w, r, a, cacheControl, "")
}

// writeAsset sends one prepared asset: gzipped for a client that
// accepts it, with an ETag, and 304 when the client's copy still
// matches. vary is the extra request header this body depends on (the
// shell's Accept-Language; "" for the other assets).
func writeAsset(w http.ResponseWriter, r *http.Request, a *asset, cacheControl, vary string) {
	h := w.Header()
	body, tag := a.plain, a.etag
	if a.compressible {
		if vary != "" {
			h.Set("Vary", vary+", Accept-Encoding")
		} else {
			h.Set("Vary", "Accept-Encoding")
		}
		if acceptsGzip(r) {
			body, tag = a.gz, a.etagGz
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", tag)
	if matchesETag(r, tag) {
		h.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", a.contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
