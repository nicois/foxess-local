// Package web serves the operator web UI (plan-2 task 11, the final
// stage): a single-page htmx 4 dashboard over /api/v1, embedded in the
// binary (C-006: pure Go, no new module dependencies — the JS assets
// are vendored files, not npm deps). There is no Go auth in this
// package: the HTML contains no data — every datum flows through
// authenticated /api/v1 calls (the same model as /healthz being open).
package web

import "embed"

// staticFS embeds the UI assets: the index shell, css, the app module,
// the favicon, and the version-pinned vendor scripts.
//
//go:embed all:static
var staticFS embed.FS

// vendorPin embeds vendor.txt: the package, version, source URL, and
// SHA-256 pin for each vendored script. The pin test parses it
// (TestVendorPin), so it is load-bearing — not a comment.
//
//go:embed vendor.txt
var vendorPin []byte
