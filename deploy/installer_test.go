package deploy

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// installerPart lifts one fragment out of the installer generator (the
// installer itself needs root; its pure pieces are tested in bash).
func installerPart(t *testing.T, re string) string {
	t.Helper()
	src, err := os.ReadFile("gen-installer.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(re).Find(src)
	if m == nil {
		t.Fatalf("installer fragment %q not found", re)
	}
	return string(m)
}

// TestInstallerValidIPv4 (install review I7): four octets, each 0-255.
func TestInstallerValidIPv4(t *testing.T) {
	fn := installerPart(t, `(?s)valid_ipv4\(\) \{.*?\n\}`)
	for addr, want := range map[string]bool{
		"192.168.1.50": true, "10.0.0.1": true, "0.0.0.0": true,
		"999.1.1.1": false, "192.168.0": false, "192.168.0.1.5": false, "a.b.c.d": false, "": false,
	} {
		//nolint:gosec // G204: a test constant
		err := exec.Command("bash", "-c", fn+"\nvalid_ipv4 \"$1\"", "_", addr).Run()
		if (err == nil) != want {
			t.Errorf("valid_ipv4 %q = %v, want %v", addr, err == nil, want)
		}
	}
}

// TestInstallerRecordsForwardMode (install review I1): the chosen mode
// replaces the commented default in the seeded config — exactly once.
func TestInstallerRecordsForwardMode(t *testing.T) {
	line := installerPart(t, `sed -i "0,/\^# mode = .*config\.toml`)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	example, err := os.ReadFile("../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, example, 0o600); err != nil { //nolint:gosec // G703: a test temp dir
		t.Fatal(err)
	}
	script := strings.ReplaceAll(line, "/etc/foxess-local/config.toml", cfg)
	//nolint:gosec // G204: a fragment of this repository's installer
	if out, err := exec.Command("bash", "-c", "FORWARD_MODE=telemetry_only; "+script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(cfg) //nolint:gosec // G304: a test temp file
	if n := strings.Count(string(got), "\nmode = \"telemetry_only\"\n"); n != 1 {
		t.Fatalf("mode lines %d, want 1", n)
	}
}

// TestInstallerPackageNames (install review I5): the package that
// provides each gate tool, per package manager.
func TestInstallerPackageNames(t *testing.T) {
	fn := installerPart(t, `(?s)pkg_for\(\) \{.*?\n\}`)
	const nftables = "nftables"
	for in, want := range map[string]string{
		"apt nft": nftables, "dnf nft": nftables, "apk nft": nftables,
		"apt ip": "iproute2", "dnf ip": "iproute", "pacman ip": "iproute2",
		"apt conntrack": "conntrack", "dnf conntrack": "conntrack-tools", "zypper conntrack": "conntrack-tools",
	} {
		pm, tool, _ := strings.Cut(in, " ")
		//nolint:gosec // G204: a fragment of this repository's installer
		out, err := exec.Command("bash", "-c", fn+"\npkg_for \"$1\" \"$2\"", "_", pm, tool).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("pkg_for %s %s = %q (%v), want %q", pm, tool, out, err, want)
		}
	}
}

// TestInstallerRefreshesAptLists (release rehearsal, 2026-11-18): a fresh
// Debian image has empty package lists, so `apt-get install` finds no
// nftables. The first apt install refreshes the lists, once.
func TestInstallerRefreshesAptLists(t *testing.T) {
	fn := installerPart(t, `(?s)pkg_install\(\) \{.*?\n\}`)
	// a fake apt-get that logs its arguments
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	fake := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "apt-get"), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: a test helper script
		t.Fatal(err)
	}
	script := "PATH=" + bin + ":$PATH\n" + fn + "\npkg_install apt nftables\npkg_install apt conntrack\n"
	//nolint:gosec // G204: a fragment of this repository's installer
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(log) //nolint:gosec // G304: a test temp file
	want := "update -qq\ninstall -y nftables\ninstall -y conntrack\n"
	if string(got) != want {
		t.Fatalf("apt-get calls:\n%s\nwant:\n%s", got, want)
	}
}

// TestInstallerDefinesBeforeUse (release rehearsal, 2026-11-18): bash runs
// the installer top to bottom, so a helper used by top-level code must be
// defined above that use. valid_ipv4 was defined below its one call, and
// every install died with "valid_ipv4: command not found"; the unit test
// of the function alone could not see it.
func TestInstallerDefinesBeforeUse(t *testing.T) {
	src, err := os.ReadFile("gen-installer.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	defined, inBody := installerFunctions(lines)
	for name, at := range defined {
		for i := range at {
			if !inBody[i] && usesCommand(lines[i], name) {
				t.Errorf("gen-installer.sh:%d uses %s, defined only at line %d", i+1, name, at+1)
			}
		}
	}
}

// installerFunctions finds each function's definition line and marks the
// lines inside function bodies.
func installerFunctions(lines []string) (map[string]int, []bool) {
	defRe := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\) \{`)
	defined := map[string]int{}
	inBody := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		m := defRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		defined[m[1]] = i
		for ; i < len(lines); i++ {
			inBody[i] = true
			if lines[i] == "}" {
				break
			}
		}
	}
	return defined, inBody
}

// usesCommand: line runs name in command position — a statement's start,
// or after ; && || | ( ! then do if else (not a word in a test such as
// [ "$CMD" = check ]); comments do not count.
func usesCommand(line, name string) bool {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return false
	}
	return regexp.MustCompile(`(^\s*|[;&|(]\s*|\b(then|do|if|else|!)\s+)` + name + `(\s|$)`).MatchString(line)
}

// TestInstallerDetectMACAbsent (release rehearsal, 2026-11-18): on a first
// install the inverter is not pointed at this machine yet, so it is not
// in the neighbour table. detect_mac must then print nothing and succeed:
// under set -euo pipefail a failing grep stopped the install silently,
// half done.
func TestInstallerDetectMACAbsent(t *testing.T) {
	fn := installerPart(t, `(?s)detect_mac\(\) \{.*?\n\}`)
	bin := t.TempDir()
	// an ip that knows no neighbours
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: a test helper script
		t.Fatal(err)
	}
	script := "set -euo pipefail\nPATH=" + bin + ":$PATH\n" + fn + "\nm=\"$(detect_mac 192.0.2.10)\"\necho \"mac=[$m]\"\n"
	//nolint:gosec // G204: a fragment of this repository's installer
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "mac=[]" {
		t.Fatalf("detect_mac with no neighbour: %v %q", err, out)
	}
}

// TestInstallerVersionOlder (release rehearsal, 2026-11-18): re-running an
// older download's installer must not silently undo a self-upgrade.
// version_older A B: A is a release older than release B; anything that
// is not a release tag (a source build) is never "older".
func TestInstallerVersionOlder(t *testing.T) {
	fn := installerPart(t, `(?s)version_older\(\) \{.*?\n\}`)
	const v1, v2 = "v0.0.1", "v0.0.2"
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{v1, v2, true},
		{"v0.2.0", "v0.10.0", true},
		{"v1.9.9", "v2.0.0", true},
		{v2, v2, false},
		{"v0.0.3", v2, false},
		{"v0.10.0", "v0.9.0", false},
		{"41c82ea", v2, false},
		{v1, "41c82ea", false},
		{"dev", v1, false},
	} {
		//nolint:gosec // G204: a fragment of this repository's installer
		err := exec.Command("bash", "-c", fn+"\nversion_older \"$1\" \"$2\"", "_", c.a, c.b).Run()
		if (err == nil) != c.want {
			t.Errorf("version_older %s %s = %v, want %v", c.a, c.b, err == nil, c.want)
		}
	}
}

// TestInstallerFreshIndexes (2026-11-18): like apt, apk has no package
// index on a fresh image, so its install refreshes it; dnf and zypper
// refresh by themselves; pacman never syncs without upgrading (a partial
// upgrade) — a failure prints the -Syu hint instead.
func TestInstallerFreshIndexes(t *testing.T) {
	fn := installerPart(t, `(?s)pkg_install\(\) \{.*?\n\}`)
	for pm, want := range map[string]string{
		"apk":    "apk add --update-cache nftables\n",
		"pacman": "pacman -S --noconfirm --needed nftables\n",
		"dnf":    "dnf install -y nftables\n",
		"zypper": "zypper --non-interactive install nftables\n",
	} {
		bin := t.TempDir()
		log := filepath.Join(bin, "log")
		if err := os.WriteFile(filepath.Join(bin, pm), []byte("#!/bin/sh\necho \"$(basename \"$0\") $*\" >> "+log+"\n"), 0o700); err != nil { //nolint:gosec // G306: a test helper script
			t.Fatal(err)
		}
		//nolint:gosec // G204: a fragment of this repository's installer
		if out, err := exec.Command("bash", "-c", "PATH="+bin+":$PATH\n"+fn+"\npkg_install "+pm+" nftables\n").CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", pm, err, out)
		}
		if got, _ := os.ReadFile(log); string(got) != want { //nolint:gosec // G304: a test temp file
			t.Errorf("%s: %q, want %q", pm, got, want)
		}
	}
}

// TestInstallerSELinuxLabel (Fedora rehearsal, 2026-11-18): the service
// cannot exec a var_lib_t binary. With semanage, a persistent rule plus
// restorecon; without it (Fedora's cloud image), chcon on the directory —
// files the app writes there later (its updates) inherit the type.
func TestInstallerSELinuxLabel(t *testing.T) {
	fn := installerPart(t, `(?s)label_bin\(\) \{.*?\n\}`)
	const semanage, chcon, restorecon = "semanage", "chcon", "restorecon"
	for name, tools := range map[string][]string{
		semanage: {semanage, restorecon, chcon},
		chcon:    {restorecon, chcon},
	} {
		bin := t.TempDir()
		log := filepath.Join(bin, "log")
		for _, tool := range tools {
			if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho \"${0##*/} $*\" >> "+log+"\n"), 0o700); err != nil { //nolint:gosec // G306: a test helper script
				t.Fatal(err)
			}
		}
		// PATH is only the stubs (and bash's builtins): an installed
		// semanage on the test host must not leak in
		//nolint:gosec // G204: a fragment of this repository's installer
		out, err := exec.Command("/bin/bash", "-c", "PATH="+bin+"\n"+fn+"\nlabel_bin /x/bin\n").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		got, _ := os.ReadFile(log) //nolint:gosec // G304: a test temp file
		want := map[string]string{
			semanage: "semanage fcontext -a -t bin_t /x/bin(/.*)?\nrestorecon -R /x/bin\n",
			chcon:    "chcon -R -t bin_t /x/bin\n",
		}[name]
		if string(got) != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	pkg := installerPart(t, `(?s)pkg_for\(\) \{.*?\n\}`)
	//nolint:gosec // G204: a fragment of this repository's installer
	if out, _ := exec.Command("bash", "-c", pkg+"\npkg_for dnf semanage").Output(); strings.TrimSpace(string(out)) != "policycoreutils-python-utils" {
		t.Errorf("pkg_for dnf semanage = %q", out)
	}
}

// TestInstallerUIProbeKeepsStderr (Fedora rehearsal, 2026-11-18): the UI
// probe's connect must not print "Connection refused" while the app
// starts, and must not redirect the installer's own stderr (a bare
// `exec 3<>… 2>/dev/null` silences the shell for good).
func TestInstallerUIProbeKeepsStderr(t *testing.T) {
	fn := installerPart(t, `(?s)ui_ok\(\) \{.*?\n\}`)
	ln, err := net.Listen("tcp", "127.0.0.1:0") // the live app may hold 18080
	if err != nil {
		t.Fatal(err)
	}
	go func() { // take the request, answer nothing (the probe reads to EOF)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// FIN first (the probe's cat sees EOF), then drain until
				// the probe closes: an unread byte at close is a reset
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fn = strings.ReplaceAll(fn, "18080", port)
	run := func() string {
		//nolint:gosec // G204: a fragment of this repository's installer
		out, _ := exec.Command("bash", "-c", fn+"\nui_ok; echo still-visible >&2\n").CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "still-visible" { // connects: stderr must survive
		t.Errorf("after a connect: %q", got)
	}
	_ = ln.Close()
	if got := run(); got != "still-visible" { // refused: nothing printed
		t.Errorf("after a refused connect: %q", got)
	}
}
