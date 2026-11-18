package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCatalogues (owner, 2026-11-18): every UI language translates every
// UI string, with the same {placeholders}, and keeps no stale entries — a
// string added or reworded in the UI fails here until each catalogue has
// it (go run ./tools/i18n lists what is missing).
func TestCatalogues(t *testing.T) {
	keys, errs := UIKeys("static")
	for _, err := range errs {
		t.Error(err)
	}
	if len(keys) < 50 {
		t.Fatalf("only %d UI strings found: the scan is broken", len(keys))
	}
	for _, lang := range UILangs {
		b, err := os.ReadFile(filepath.Join("static", "i18n", lang+".json")) //nolint:gosec // G304: the package's own catalogues
		if err != nil {
			t.Fatal(err)
		}
		var cat map[string]string
		if err := json.Unmarshal(b, &cat); err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		missing, mismatched, stale := CatalogueProblems(keys, cat)
		for _, m := range missing {
			t.Errorf("%s: missing %s", lang, m)
		}
		for _, m := range mismatched {
			t.Errorf("%s: %s", lang, m)
		}
		for _, s := range stale {
			t.Errorf("%s: stale entry %s (no longer in the UI)", lang, s)
		}
	}
}
