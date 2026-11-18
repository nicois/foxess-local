// Command i18n reports the web UI's translation catalogues against the
// UI's strings (internal/web/static/i18n/<lang>.json):
//
//	go run ./tools/i18n            # per language: missing, mismatched and stale keys
//	go run ./tools/i18n -keys      # every UI string, as a JSON array
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"foxesslocal/internal/web"
)

const static = "internal/web/static"

func main() {
	list := flag.Bool("keys", false, "print every UI string as a JSON array")
	flag.Parse()
	keys, errs := web.UIKeys(static)
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, err)
	}
	if *list {
		printKeys(keys)
		return
	}
	for _, lang := range web.UILangs {
		report(lang, keys)
	}
}

func printKeys(keys map[string]string) {
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	slices.Sort(all)
	out, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func report(lang string, keys map[string]string) {
	cat := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(static, "i18n", lang+".json")); err == nil { //nolint:gosec // G304: the repository's catalogues
		_ = json.Unmarshal(b, &cat)
	}
	missing, mismatched, stale := web.CatalogueProblems(keys, cat)
	for _, m := range missing {
		fmt.Printf("%s missing %s\n", lang, m)
	}
	for _, m := range mismatched {
		fmt.Printf("%s mismatched %s\n", lang, m)
	}
	for _, s := range stale {
		fmt.Printf("%s stale %s\n", lang, s)
	}
	fmt.Printf("%s: %d of %d missing\n", lang, len(missing), len(keys))
}
