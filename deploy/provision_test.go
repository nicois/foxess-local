package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// provisionPart lifts one fragment out of provision-openstick.sh (the
// script itself needs root and an MSM8916 stick).
func provisionPart(t *testing.T, re string) string {
	t.Helper()
	src, err := os.ReadFile("provision-openstick.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(re).Find(src)
	if m == nil {
		t.Fatalf("provision fragment %q not found", re)
	}
	return string(m)
}

// TestProvisionKey: the OpenStick script verifies the release it
// installs against the key every binary carries.
func TestProvisionKey(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(ReleaseKey)), "\n")
	want := `RELEASE_KEY="` + strings.TrimSpace(lines[len(lines)-1]) + `"`
	if got := provisionPart(t, `RELEASE_KEY="[^"]*"`); got != want {
		t.Fatalf("provision-openstick.sh has %s, want %s", got, want)
	}
}

// TestProvisionPinSuites: "stable" (which moved to a newer Debian under
// the OpenStick image) is pinned to the installed codename, in both
// sources formats; other lines and a second run change nothing.
func TestProvisionPinSuites(t *testing.T) {
	fn := provisionPart(t, `(?s)pin_suites\(\) \{.*?\n\}`)
	cases := map[string][2]string{
		"sources.list": {
			"deb http://deb.debian.org/debian stable main contrib non-free-firmware\n" +
				"deb http://deb.debian.org/debian-security/ stable-security main\n" +
				"deb [arch=arm64] http://deb.debian.org/debian stable-updates main\n" +
				"# deb http://deb.debian.org/debian stable main\n" +
				"deb http://example.org/stable bookworm main\n",
			"deb http://deb.debian.org/debian bookworm main contrib non-free-firmware\n" +
				"deb http://deb.debian.org/debian-security/ bookworm-security main\n" +
				"deb [arch=arm64] http://deb.debian.org/debian bookworm-updates main\n" +
				"# deb http://deb.debian.org/debian stable main\n" +
				"deb http://example.org/stable bookworm main\n",
		},
		"debian.sources": {
			"Types: deb\nURIs: http://deb.debian.org/debian\nSuites: stable stable-updates\nComponents: main stable\n",
			"Types: deb\nURIs: http://deb.debian.org/debian\nSuites: bookworm bookworm-updates\nComponents: main stable\n",
		},
	}
	for name, c := range cases {
		f := filepath.Join(t.TempDir(), name)
		got := c[0]
		for range 2 { // idempotent
			if err := os.WriteFile(f, []byte(got), 0o600); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // G204: a fragment of this repository's script
			out, err := exec.Command("bash", "-c", "CODENAME=bookworm\n"+fn+"\npin_suites \"$1\"", "_", f).Output()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got = string(out)
			if got != c[1] {
				t.Fatalf("%s:\n%s\nwant:\n%s", name, got, c[1])
			}
		}
	}
}
