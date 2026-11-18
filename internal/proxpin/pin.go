// Package proxpin names the prox version this build compiles (audit M2):
// read from the build info, so the version stamp every stored row
// carries, the conformance suite and go.mod cannot disagree.
package proxpin

import (
	"runtime/debug"
	"strings"
	"sync"
)

// modulePath is prox's module path.
const modulePath = "github.com/nicois/prox"

// unknown is the stamp when the build carries no prox version.
const unknown = "unknown"

// commitLen is the short commit form.
const commitLen = 7

// pseudoParts: a pseudo-version is <base>-<timestamp>-<12-hex commit>.
const pseudoParts = 3

var version = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path == modulePath {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return ""
})

// Version is the prox module version compiled in ("" when unknown).
func Version() string { return version() }

// Short is the version for the stamp: the commit of a pseudo-version,
// else the tag.
func Short() string { return shortOf(Version()) }

func shortOf(v string) string {
	if v == "" || !strings.HasPrefix(v, "v") {
		return unknown
	}
	parts := strings.Split(v, "-")
	if len(parts) >= pseudoParts {
		if c := parts[len(parts)-1]; len(c) == 12 {
			return c[:commitLen]
		}
	}
	return v
}
