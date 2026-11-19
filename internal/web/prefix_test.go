package web

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestNoRootRelativePaths (2026-11-19): the UI works behind a reverse
// proxy that serves it under a path prefix (nginx: location /foxess/ {
// proxy_pass http://host:18080/; }). Every URL the page loads or calls
// is relative to the page, never "/static/…" or "/api/…" (those would
// leave the prefix).
func TestNoRootRelativePaths(t *testing.T) {
	rootRel := regexp.MustCompile(`(?:["'\x60(=]\s*)/(?:static|api)/`)
	files, _ := filepath.Glob("static/js/*.js")
	files = append(files, "static/index.html")
	css, _ := filepath.Glob("static/css/*.css")
	files = append(files, css...)
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: the package's own static files
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rootRel.FindAllIndex(b, -1) {
			line := 1
			for _, c := range b[:m[0]] {
				if c == '\n' {
					line++
				}
			}
			t.Errorf("%s:%d: a root-relative path (%q) breaks a proxy prefix", f, line, b[m[0]:m[1]])
		}
	}
}
