// server_test.go: the task-11 serving contract — the starter CSP and
// security headers on every response, the vendor pin (the embedded
// SHA-256s equal the ones parsed from vendor.txt), the immutable vs
// no-cache split, and the 404 (no index fallback).
package web

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cspStarter is the brief's starter CSP (no 'unsafe-inline', nothing
// external in connect-src — LAN-only by design).
const cspStarter = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"

const (
	ctJS  = "application/javascript"
	ctCSS = "text/css"
)

func doGET(t *testing.T, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// checkSecurityHeaders asserts the three headers on every response
// (including 404s).
func checkSecurityHeaders(t *testing.T, r *http.Response) {
	t.Helper()
	if got := r.Header.Get("Content-Security-Policy"); got != cspStarter {
		t.Errorf("Content-Security-Policy = %q, want the starter CSP", got)
	}
	if got := r.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := r.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestIndex(t *testing.T) {
	r := doGET(t, "/")
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", r.StatusCode)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html(+charset)", ct)
	}
	if cc := r.Header.Get("Cache-Control"); cc != cacheAssets {
		t.Errorf("Cache-Control = %q, want %q", cc, cacheAssets)
	}
	checkSecurityHeaders(t, r)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	vendor := "static/" + tagPrefix + uiTag() + "/vendor/"
	for _, ref := range []string{`src="` + vendor + `htmx.min.js"`, `src="` + vendor + `hx-sse.min.js"`} { // relative: a proxy prefix works
		if !strings.Contains(string(body), ref) {
			t.Errorf("index.html does not reference the vendored script %s", ref)
		}
	}
}

func TestIndexAlias(t *testing.T) {
	r := doGET(t, "/index.html")
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /index.html status = %d, want 200", r.StatusCode)
	}
}

func TestStaticAssets(t *testing.T) {
	cases := []struct {
		path string
		ct   string
		cc   string
	}{
		{"/static/vendor/htmx.min.js", ctJS, cacheVendor},
		{"/static/vendor/hx-sse.min.js", ctJS, cacheVendor},
		{"/static/vendor/apexcharts.min.js", ctJS, cacheVendor},
		{"/static/vendor/apexcharts.css", ctCSS, cacheVendor},
		{"/static/vendor/apexcharts-legend.css", ctCSS, cacheVendor},
		{pathAppJS, ctJS, cacheAssets},
		{"/static/css/app.css", ctCSS, cacheAssets},
		{"/static/index.html", "text/html", cacheAssets},
		{"/static/favicon.svg", "image/svg+xml", cacheAssets},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			r := doGET(t, tc.path)
			defer func() { _ = r.Body.Close() }()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", r.StatusCode)
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.ct) {
				t.Errorf("Content-Type = %q, want %q(+charset)", ct, tc.ct)
			}
			if cc := r.Header.Get("Cache-Control"); cc != tc.cc {
				t.Errorf("Cache-Control = %q, want %q", cc, tc.cc)
			}
			checkSecurityHeaders(t, r)
		})
	}
}

// vendorPackage is one package block of vendor.txt.
type vendorPackage struct {
	Package string
	Version string
	Source  string
	Pins    map[string]string
}

// parseVendorPin parses the embedded vendor.txt (key: value lines,
// comments and blanks skipped); a block starts at a "package:" line and
// its non-key lines are SHA-256 pins (path: hash).
//
//nolint:gocognit // one case per vendor.txt key
func parseVendorPin() ([]vendorPackage, error) {
	var out []vendorPackage
	var cur *vendorPackage
	scanner := bufio.NewScanner(strings.NewReader(string(vendorPin)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("unparseable vendor.txt line %q", line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "package":
			out = append(out, vendorPackage{Package: value, Pins: map[string]string{}})
			cur = &out[len(out)-1]
		case "version":
			if cur == nil {
				return nil, fmt.Errorf("vendor.txt: version before a package block")
			}
			cur.Version = value
		case "source":
			if cur == nil {
				return nil, fmt.Errorf("vendor.txt: source before a package block")
			}
			cur.Source = value
		default:
			if cur == nil {
				return nil, fmt.Errorf("vendor.txt: pin before a package block")
			}
			cur.Pins[key] = value
		}
	}
	return out, scanner.Err()
}

// TestVendorPin asserts the embedded SHA-256 of each vendored file equals
// the pin parsed from vendor.txt (parsed, not copy-pasted).
//
//nolint:gocognit // one assertion per vendor.txt invariant
func TestVendorPin(t *testing.T) {
	pkgs, err := parseVendorPin()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("vendor.txt must record exactly 2 packages, got %d", len(pkgs))
	}
	for _, p := range pkgs {
		if p.Package == "" || p.Version == "" || p.Source == "" {
			t.Fatalf("package %q: must record package, version and source", p.Package)
		}
		if len(p.Pins) == 0 {
			t.Fatalf("package %q: must pin at least one file", p.Package)
		}
		for path, want := range p.Pins {
			t.Run(p.Package+"/"+path, func(t *testing.T) {
				data, err := staticFS.ReadFile(path)
				if err != nil {
					t.Fatalf("read embedded %s: %v", path, err)
				}
				got := sha256.Sum256(data)
				if hex.EncodeToString(got[:]) != want {
					t.Errorf("SHA-256 of %s = %s, vendor.txt pins %s", path, hex.EncodeToString(got[:]), want)
				}
			})
		}
	}
}

func TestNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/static/nope.js", "/static/vendor/nope.min.js", "/static/js/../../nope"} {
		t.Run(path, func(t *testing.T) {
			r := doGET(t, path)
			defer func() { _ = r.Body.Close() }()
			if r.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404 (no index fallback)", path, r.StatusCode)
			}
			checkSecurityHeaders(t, r)
		})
	}
}
