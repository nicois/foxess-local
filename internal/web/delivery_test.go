// delivery_test.go: how the assets travel (2026-11-20, a Lighthouse run
// through a reverse proxy: app.js crossed the network uncompressed and
// every visit downloaded every asset again). Text assets are gzipped for
// a client that accepts it, every asset carries an ETag, and a
// revalidation that still matches answers 304 with no body.
package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// pathAppJS is the app module's URL.
const pathAppJS = "/static/js/app.js"

func doReq(t *testing.T, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// TestGzip: a client that accepts gzip gets the text assets compressed
// (smaller, and identical once inflated); one that does not gets them
// plain. Either way the response varies on Accept-Encoding.
func TestGzip(t *testing.T) {
	for _, path := range []string{"/", pathAppJS, "/static/css/app.css", "/static/vendor/htmx.min.js"} {
		plain := doReq(t, path, nil)
		want, _ := io.ReadAll(plain.Body)
		_ = plain.Body.Close()
		if plain.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s without Accept-Encoding: Content-Encoding %q", path, plain.Header.Get("Content-Encoding"))
		}
		r := doReq(t, path, map[string]string{"Accept-Encoding": "gzip, deflate, br"})
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: Content-Encoding %q, want gzip", path, r.Header.Get("Content-Encoding"))
		}
		if !strings.Contains(r.Header.Get("Vary"), "Accept-Encoding") || !strings.Contains(plain.Header.Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s: Vary %q / %q, want Accept-Encoding on both", path, r.Header.Get("Vary"), plain.Header.Get("Vary"))
		}
		if len(body) >= len(want) {
			t.Errorf("%s: gzipped %d bytes, plain %d", path, len(body), len(want))
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got, _ := io.ReadAll(zr)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: inflated body differs from the plain one", path)
		}
		checkSecurityHeaders(t, r)
	}
}

// TestETagRevalidation: each asset has an ETag; sending it back answers
// 304 without a body (the browser reuses its copy), and a stale one
// gets the full asset. The plain and gzipped forms have distinct tags.
func TestETagRevalidation(t *testing.T) {
	for _, path := range []string{"/", pathAppJS, "/static/favicon.svg"} {
		r := doReq(t, path, nil)
		_ = r.Body.Close()
		tag := r.Header.Get("ETag")
		if tag == "" || !strings.HasPrefix(tag, `"`) {
			t.Fatalf("%s: ETag %q, want a quoted tag", path, tag)
		}
		nm := doReq(t, path, map[string]string{"If-None-Match": tag})
		body, _ := io.ReadAll(nm.Body)
		_ = nm.Body.Close()
		if nm.StatusCode != http.StatusNotModified || len(body) != 0 {
			t.Errorf("%s revalidated: %d with %d bytes, want 304 and none", path, nm.StatusCode, len(body))
		}
		if nm.Header.Get("ETag") != tag || nm.Header.Get("Cache-Control") == "" {
			t.Errorf("%s 304: ETag %q Cache-Control %q", path, nm.Header.Get("ETag"), nm.Header.Get("Cache-Control"))
		}
		stale := doReq(t, path, map[string]string{"If-None-Match": `"stale"`})
		_ = stale.Body.Close()
		if stale.StatusCode != http.StatusOK {
			t.Errorf("%s with a stale tag: %d, want 200", path, stale.StatusCode)
		}
	}
	zr := doReq(t, pathAppJS, map[string]string{"Accept-Encoding": "gzip"})
	_ = zr.Body.Close()
	pl := doReq(t, pathAppJS, nil)
	_ = pl.Body.Close()
	if zr.Header.Get("ETag") == pl.Header.Get("ETag") {
		t.Error("the gzipped and plain app.js share an ETag")
	}
}

// TestModulePreload: the shell names every module app.js imports, so
// the browser fetches them alongside app.js instead of after it.
func TestModulePreload(t *testing.T) {
	r := doGET(t, "/")
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	app, err := os.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	imports := regexp.MustCompile(`(?m)^import .* from "\./([a-z0-9]+\.js)";`).FindAllStringSubmatch(string(app), -1)
	if len(imports) < 3 {
		t.Fatalf("app.js imports: found %d, want i18n.js, charts.js, socview.js at least", len(imports))
	}
	for _, im := range imports {
		m := "static/js/" + im[1]
		if !strings.Contains(string(body), `<link rel="modulepreload" href="`+m+`">`) {
			t.Errorf("index.html does not preload %s", m)
		}
	}
}
