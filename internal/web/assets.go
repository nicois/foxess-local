package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"sync"
)

// asset is one embedded file ready to send: its bytes, a gzipped copy
// for the text types, and an ETag for each form. The embedded files
// never change while the process runs, so each is prepared once, on
// first request.
type asset struct {
	plain, gz    []byte
	etag, etagGz string
	compressible bool
	contentType  string
}

const htmlOpen = `<html lang="en">`

var (
	assetsMu sync.Mutex
	assets   = map[string]*asset{}
)

// compressibleExt: the text types worth gzipping (the favicon SVG too).
var compressibleExt = map[string]bool{".html": true, ".js": true, ".css": true, ".svg": true, ".json": true, ".txt": true}

// etagLen is how many hex digits of the SHA-256 the ETag keeps.
const etagLen = 20

// loadAsset prepares one embedded file (nil when it does not exist).
func loadAsset(name string) *asset {
	assetsMu.Lock()
	defer assetsMu.Unlock()
	if a, ok := assets[name]; ok {
		return a
	}
	data, err := staticFS.ReadFile(name)
	if err != nil {
		return nil
	}
	if name == "static/index.html" {
		data = tagShell(data)
	}
	assets[name] = prepare(data, name)
	return assets[name]
}

// loadHintShell prepares the index shell with data-lang-hint="<hint>"
// on the <html> tag (nil when the shell cannot be read): the request's
// Accept-Language, so a cached copy never shows the wrong language.
func loadHintShell(hint string) *asset {
	assetsMu.Lock()
	defer assetsMu.Unlock()
	name := "static/index.html?hint=" + hint
	if a, ok := assets[name]; ok {
		return a
	}
	base, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil
	}
	base = tagShell(base)
	body := base
	if before, after, ok := bytes.Cut(base, []byte(htmlOpen)); ok {
		open := `<html lang="en" data-lang-hint="` + hint + `">`
		body = make([]byte, 0, len(base)+len(open)-len(htmlOpen))
		body = append(body, before...)
		body = append(body, open...)
		body = append(body, after...)
	}
	assets[name] = prepare(body, "static/index.html")
	return assets[name]
}

// prepare turns one file's bytes into a sendable asset: the ETag is a
// SHA-256 prefix of the exact bytes sent, so two bodies never share a
// tag (the hint shell variants revalidate against their own body).
func prepare(data []byte, name string) *asset {
	sum := sha256.Sum256(data)
	tag := hex.EncodeToString(sum[:])[:etagLen]
	a := &asset{plain: data, etag: `"` + tag + `"`, contentType: contentType(name)}
	if compressibleExt[strings.ToLower(path.Ext(name))] {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression) // a valid level: no error
		if _, err := zw.Write(data); err == nil && zw.Close() == nil && b.Len() < len(data) {
			a.compressible = true
			a.gz = b.Bytes()
			a.etagGz = `"` + tag + `-gz"`
		}
	}
	return a
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip
// (absent, or with q=0, does not).
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") || enc == "*" {
			return strings.ReplaceAll(params, " ", "") != "q=0"
		}
	}
	return false
}

// matchesETag reports whether If-None-Match names tag (or is "*").
func matchesETag(r *http.Request, tag string) bool {
	for t := range strings.SplitSeq(r.Header.Get("If-None-Match"), ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == tag || t == "*" {
			return true
		}
	}
	return false
}
