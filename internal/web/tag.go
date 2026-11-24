package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"sync"
)

// The UI tag (owner, 2026-11-25): a browser may reuse a cached asset
// without revalidating it (back/forward, a restored or discarded tab),
// so a page could run an older release's code against this server. The
// shell names every asset under static/v-<tag>/, the tag a hash of the
// whole embedded UI: a release that changes any file changes every URL.
// The page also asks for the tag (static/ui-tag) and reloads when its
// own differs.

// tagPrefix is the path segment that carries the tag.
const tagPrefix = "v-"

// tagLen is how many hex digits of the SHA-256 the tag keeps.
const tagLen = 16

var uiTag = sync.OnceValue(func() string {
	h := sha256.New()
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := staticFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:tagLen]
})

// tagShell points the shell's asset references (href="static/…",
// src="static/…") at the current tag.
func tagShell(b []byte) []byte {
	tagged := `static/` + tagPrefix + uiTag() + `/`
	for _, attr := range []string{`href="`, `src="`} {
		b = bytes.ReplaceAll(b, []byte(attr+`static/`), []byte(attr+tagged))
	}
	return b
}

// splitTag takes the tag off a static path: "static/v-<tag>/js/app.js"
// → ("static/js/app.js", "<tag>", true); untagged paths are returned
// as they are.
func splitTag(name string) (string, string, bool) {
	rest, ok := strings.CutPrefix(name, "static/"+tagPrefix)
	if !ok {
		return name, "", false
	}
	tag, file, ok := strings.Cut(rest, "/")
	if !ok || tag == "" {
		return name, "", false
	}
	return "static/" + file, tag, true
}
