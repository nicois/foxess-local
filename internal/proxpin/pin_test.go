package proxpin

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// linked so this test binary's build info lists prox, as the
	// service binary's does.
	_ "github.com/nicois/prox/pkg/capture"
)

// TestShortOf: a pseudo-version shortens to its 7-character commit, a
// release tag stays as it is, anything else is "unknown".
func TestShortOf(t *testing.T) {
	for in, want := range map[string]string{
		"v0.0.0-20261113075658-359e3da207e2":   "359e3da",
		"v0.1.1-0.20261201010101-abcdef123456": "abcdef1",
		"v0.1.0":                               "v0.1.0",
		"v1.2.3+incompatible":                  "v1.2.3+incompatible",
		"":                                     "unknown",
		"(devel)":                              "unknown",
	} {
		if got := shortOf(in); got != want {
			t.Errorf("shortOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVersionIsTheRequirement (audit M2): the stamp is the prox version
// the build compiles — go.mod's requirement, read from the build info.
func TestVersionIsTheRequirement(t *testing.T) {
	want := requirement(t)
	if got := Version(); got != want {
		t.Fatalf("Version() = %q, go.mod requires %q", got, want)
	}
	if Short() != shortOf(want) {
		t.Fatalf("Short() = %q", Short())
	}
}

// requirement is go.mod's prox version.
func requirement(t *testing.T) string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == modulePath {
			return fields[1]
		}
	}
	t.Fatal("go.mod does not require prox")
	return ""
}
