// cachebust_test.go (owner, 2026-11-25): a phone showed rc.3's
// diagram from an rc.6 server. A browser may reuse a cached asset
// without asking (back/forward, a restored or discarded tab), and the
// assets kept one URL across releases, so the shell names them under a
// tag of the embedded UI's content: a new release is new URLs. A tagged
// asset is immutable; an old tag gets today's file, never cached.
package web

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var tagRef = regexp.MustCompile(`static/v-([0-9a-f]+)/`)

// get is one GET: its body (read and closed) and its headers.
func get(t *testing.T, path string) (string, int, http.Header) {
	t.Helper()
	r := doGET(t, path)
	defer func() { _ = r.Body.Close() }()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), r.StatusCode, r.Header
}

// TestShellNamesTaggedAssets: every asset the shell loads is under the
// current tag; none is named untagged.
func TestShellNamesTaggedAssets(t *testing.T) {
	for _, lang := range []string{"", "de-DE,de"} {
		body := string(getShell(t, lang, "").body)
		tags := tagRef.FindAllStringSubmatch(body, -1)
		if len(tags) < 8 {
			t.Fatalf("lang %q: %d tagged references, want every asset (8)", lang, len(tags))
		}
		for _, m := range tags {
			if m[1] != uiTag() {
				t.Fatalf("lang %q: tag %s, want %s", lang, m[1], uiTag())
			}
		}
		if strings.Contains(body, `"static/js/`) || strings.Contains(body, `"static/css/`) {
			t.Fatalf("lang %q: an untagged asset reference remains", lang)
		}
	}
}

// TestTaggedAssetImmutable: the current tag serves the embedded file,
// cached for good.
func TestTaggedAssetImmutable(t *testing.T) {
	plain, _, _ := get(t, pathAppJS)
	got, _, h := get(t, "/static/v-"+uiTag()+"/js/app.js")
	if cc := h.Get("Cache-Control"); cc != cacheVendor {
		t.Errorf("Cache-Control = %q, want %q", cc, cacheVendor)
	}
	if got != plain {
		t.Fatal("the tagged app.js differs from the embedded one")
	}
}

// TestOldTagNotCached: another tag (a page from an older release) gets
// today's file, which no cache keeps under that name.
func TestOldTagNotCached(t *testing.T) {
	got, status, h := get(t, "/static/v-0123456789/js/app.js")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if want, _, _ := get(t, pathAppJS); got != want {
		t.Fatal("an old tag did not get today's app.js")
	}
	if _, status, _ := get(t, "/static/v-"+uiTag()+"/js/missing.js"); status != http.StatusNotFound {
		t.Fatalf("a missing tagged file: %d, want 404", status)
	}
}

// TestUITagEndpoint: the page asks for the current tag (never cached)
// and reloads when its own differs.
func TestUITagEndpoint(t *testing.T) {
	body, _, h := get(t, "/static/ui-tag")
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if got := strings.TrimSpace(body); got != uiTag() || len(got) < 12 {
		t.Fatalf("ui-tag = %q, want %q", got, uiTag())
	}
}
