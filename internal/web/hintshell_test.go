package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shellResult: one GET / with the given Accept-Language and
// If-None-Match ("" = absent), body already read.
type shellResult struct {
	status int
	body   []byte
	etag   string
	vary   []string
}

func getShell(t *testing.T, acceptLanguage, ifNoneMatch string) shellResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return shellResult{rec.Code, body, rec.Result().Header.Get("ETag"), rec.Result().Header.Values("Vary")}
}

// TestShellLangHint (owner, 2026-11-24): the shell carries the
// request's Accept-Language as data-lang-hint — with the Vary header,
// a per-variant ETag, and 304 revalidation against it.
func TestShellLangHint(t *testing.T) {
	plain := getShell(t, "", "")
	if plain.status != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", plain.status)
	}
	if strings.Contains(string(plain.body), "data-lang-hint") {
		t.Error("shell without a supported Accept-Language carries a lang hint")
	}
	if !containsAll(plain.vary, "Accept-Language") {
		t.Errorf("Vary = %v, want Accept-Language", plain.vary)
	}

	hint := getShell(t, "de-DE,de;q=0.9,en;q=0.5", "")
	if hint.status != http.StatusOK {
		t.Fatalf("GET / (de) status = %d, want 200", hint.status)
	}
	if !strings.Contains(string(hint.body), `<html lang="en" data-lang-hint="de">`) {
		t.Errorf("de shell misses the hint tag; head is %q", string(hint.body[:min(len(hint.body), 120)]))
	}
	if hint.etag == "" || hint.etag == plain.etag {
		t.Errorf("de shell ETag = %q, want set and distinct from %q", hint.etag, plain.etag)
	}
	// the hint variant is the rest of the same shell
	if len(hint.body) <= len(plain.body) {
		t.Errorf("de shell shorter than the plain one (%d <= %d)", len(hint.body), len(plain.body))
	}

	// revalidating with the variant's own tag → 304
	re := getShell(t, "de", hint.etag)
	if re.status != http.StatusNotModified {
		t.Errorf("revalidating the de shell = %d, want 304", re.status)
	}
	// …and the plain tag against the de shell → a fresh body
	fresh := getShell(t, "de", plain.etag)
	if fresh.status != http.StatusOK || !strings.Contains(string(fresh.body), `data-lang-hint="de"`) {
		t.Errorf("plain tag vs de shell: status %d, hint present = %v", fresh.status, strings.Contains(string(fresh.body), `data-lang-hint="de"`))
	}
}

func containsAll(v []string, want string) bool {
	for _, s := range v {
		for part := range strings.SplitSeq(s, ",") {
			if strings.TrimSpace(part) == want {
				return true
			}
		}
	}
	return false
}
