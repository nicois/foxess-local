package web

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// UILangs are the web UI's languages besides English (static/js/i18n.js
// LANGS; owner, 2026-11-18).
var UILangs = []string{"de", "fr", "it", "es", "zh-Hans"}

var (
	// tr("literal") — the only form a key may take (a scanner must see it).
	trCall = regexp.MustCompile(`\btr\(\s*"((?:[^"\\]|\\.)*)"`)
	// tr( followed by anything but a double quote: a key no scan can see.
	trDynamic = regexp.MustCompile(`\btr\(\s*[^"\s)]`)
	dataKey   = regexp.MustCompile(`data-i18n(?:-aria-label|-title|-placeholder)?="([^"]*)"`)
	holder    = regexp.MustCompile(`\{(\w+)\}`)
)

// UIKeys collects every translatable string under the static directory:
// tr("…") in the scripts and data-i18n* in index.html (key → where it
// was first seen). A tr() call without a literal key is an error.
func UIKeys(static string) (map[string]string, []error) {
	keys := map[string]string{}
	var errs []error
	scripts, _ := filepath.Glob(filepath.Join(static, "js", "*.js"))
	for _, f := range scripts {
		b, err := os.ReadFile(f) //nolint:gosec // G304: the package's own static files
		if err != nil {
			return nil, []error{err}
		}
		errs = append(errs, scriptKeys(keys, f, string(b))...)
	}
	html, err := os.ReadFile(filepath.Join(static, "index.html")) //nolint:gosec // G304: the package's own static file
	if err != nil {
		return nil, []error{err}
	}
	unescape := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`)
	for _, m := range dataKey.FindAllStringSubmatch(string(html), -1) {
		addKey(keys, unescape.Replace(m[1]), "index.html")
	}
	return keys, errs
}

// scriptKeys adds one script's tr("…") keys; comment lines are skipped
// (an example in a comment is not a UI string).
func scriptKeys(keys map[string]string, file, src string) []error {
	var errs []error
	base := filepath.Base(file)
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		// i18n.js is the implementation: it translates index.html's
		// marked elements by their attribute values
		if m := trDynamic.FindString(line); m != "" && base != "i18n.js" {
			errs = append(errs, fmt.Errorf("%s:%d: tr() needs a double-quoted literal (no scan can see %q)", file, i+1, m))
		}
		for _, m := range trCall.FindAllStringSubmatch(line, -1) {
			k, err := strconv.Unquote(`"` + m[1] + `"`)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s:%d: key %q: %w", file, i+1, m[1], err))
				continue
			}
			addKey(keys, k, base+":"+strconv.Itoa(i+1))
		}
	}
	return errs
}

func addKey(keys map[string]string, k, where string) {
	if _, ok := keys[k]; !ok {
		keys[k] = where
	}
}

// Placeholders are a string's {names}, sorted.
func Placeholders(s string) []string {
	var out []string
	for _, m := range holder.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return out
}

// CatalogueProblems compares one language's catalogue with the UI's keys:
// missing or empty entries, placeholder mismatches, stale entries.
func CatalogueProblems(keys, cat map[string]string) (missing, mismatched, stale []string) {
	for k, where := range keys {
		v, ok := cat[k]
		if !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, where+" "+strconv.Quote(k))
			continue
		}
		if !slices.Equal(Placeholders(k), Placeholders(v)) {
			mismatched = append(mismatched, fmt.Sprintf("%q: placeholders %v, want %v", k, Placeholders(v), Placeholders(k)))
		}
	}
	for k := range cat {
		if _, ok := keys[k]; !ok {
			stale = append(stale, strconv.Quote(k))
		}
	}
	slices.Sort(missing)
	slices.Sort(mismatched)
	slices.Sort(stale)
	return missing, mismatched, stale
}
