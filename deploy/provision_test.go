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
	src, err := os.ReadFile(provisionScript)
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

const (
	provisionScript = "provision-openstick.sh"
	applianceFlag   = "--appliance"
)

// TestProvisionArgs (D-031): the appliance modes' option rules, checked
// before anything else (so here without root).
func TestProvisionArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{applianceFlag, "--inverter-ip", "192.0.2.10"}, "--appliance installs without an inverter"},
		{[]string{"--seal", applianceFlag}, "--seal is a separate, last run"},
		{[]string{"--seal", "--inverter-ip", "192.0.2.10"}, "--seal is a separate, last run"},
		{[]string{"--allow-unsigned"}, "--allow-unsigned needs --from"},
		{[]string{"--from", "/nonexistent", applianceFlag}, "--from: /nonexistent is not a directory"},
	} {
		//nolint:gosec // G204: this repository's script with test arguments
		out, err := exec.Command("bash", append([]string{provisionScript}, c.args...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%v: %v %q, want %q", c.args, err, out, c.want)
		}
	}
}

// TestProvisionWizardUnit: the wizard runs as root from its root-owned
// directory, restarts when it stops, and gets its run and state
// directories from systemd (dnsmasq's lease file lives in the first).
func TestProvisionWizardUnit(t *testing.T) {
	unit := provisionPart(t, `(?s)WIZARD_UNIT='.*?'`)
	for _, want := range []string{
		"ExecStart=/usr/local/lib/foxess-wizard/foxess-wizard run\n",
		"Restart=always\n", "RuntimeDirectory=foxess-wizard\n", "StateDirectory=foxess-wizard\n",
		"StateDirectoryMode=0700\n", "After=NetworkManager.service\n", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the wizard unit lacks %q:\n%s", want, unit)
		}
	}
}

// TestProvisionClientWiFi (D-031): --seal removes the Wi-Fi client
// connections (the preparer's network) and nothing else: not the
// image's or the wizard's hotspots, not other connection types.
func TestProvisionClientWiFi(t *testing.T) {
	fn := provisionPart(t, `(?s)client_wifi\(\) \{.*?\n\}`)
	bin := t.TempDir()
	fake := `#!/bin/sh
case "$*" in
"-t -f UUID,TYPE con show") printf 'u-client:802-11-wireless\nu-hot:802-11-wireless\nu-usb:802-3-ethernet\nu-wiz:802-11-wireless\nu-try:802-11-wireless\n' ;;
"-g 802-11-wireless.mode con show u-client") echo infrastructure ;;
"-g 802-11-wireless.mode con show u-try") echo infrastructure ;;
"-g 802-11-wireless.mode con show u-hot") echo ap ;;
"-g 802-11-wireless.mode con show u-wiz") echo ap ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "nmcli"), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: a test helper script
		t.Fatal(err)
	}
	script := "set -euo pipefail\nPATH=" + bin + ":$PATH\n" + fn + "\necho \"[$(client_wifi | tr '\\n' ' ')]\"\n"
	//nolint:gosec // G204: a fragment of this repository's script
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "[u-client u-try ]" {
		t.Fatalf("client_wifi: %v %q", err, out)
	}
}

// TestProvisionRandomPassword: --seal's replacement for a well-known
// account password — long, random, and no failure under pipefail.
func TestProvisionRandomPassword(t *testing.T) {
	fn := provisionPart(t, `(?s)random_password\(\) \{.*?\n\}`)
	seen := map[string]bool{}
	for range 3 {
		//nolint:gosec // G204: a fragment of this repository's script
		out, err := exec.Command("bash", "-c", "set -euo pipefail\n"+fn+"\nrandom_password").Output()
		p := strings.TrimSpace(string(out))
		if err != nil || !regexp.MustCompile(`^[A-Z2-7]{20}$`).MatchString(p) || seen[p] {
			t.Fatalf("random_password: %v %q", err, p)
		}
		seen[p] = true
	}
}
