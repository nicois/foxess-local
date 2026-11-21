package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDist builds a release directory whose binaries carry version (as
// release.sh's -X main.version stamp leaves it, among other bytes) and
// a SHA256SUMS over them.
func fakeDist(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"foxess-local_linux_amd64": "\x7fELF…go1.27…v1.11.1…" + version + "…github.com/nicois/foxess-local",
		"foxess-local_linux_arm64": "\x7fELF…" + version + "…",
		"foxess-local_linux_armv7": "\x7fELF…" + version,
		"installer.sh":             "#!/bin/sh\n",
		"provision-openstick.sh":   "#!/bin/sh\n",
	}
	var sums strings.Builder
	for _, name := range []string{"foxess-local_linux_amd64", "foxess-local_linux_arm64", "foxess-local_linux_armv7", "installer.sh", "provision-openstick.sh"} {
		write(t, filepath.Join(dir, name), files[name])
		h := sha256.Sum256([]byte(files[name]))
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + name + "\n")
	}
	write(t, filepath.Join(dir, "SHA256SUMS"), sums.String())
	return dir
}

// notTag is the refusal for a VERSION that is not a release tag.
const notTag = "not a release tag"

func runSignCheck(t *testing.T, version, dist string) (string, error) {
	t.Helper()
	out, err := exec.Command("bash", "sign-check.sh", version, dist).CombinedOutput() //nolint:gosec // G204: the test's own arguments
	return string(out), err
}

// TestSignCheck: make sign refuses unless VERSION is a release tag and
// every binary it is about to sign carries that version — the slips of
// 2026-11-21 (a stale dist signed under the next version's label, and a
// mistyped v0.4.1.rc.1). It reads the files and never runs them: the
// signing account must not execute what the build account wrote.
func TestSignCheck(t *testing.T) {
	dist := fakeDist(t, "v0.4.1-rc.1")
	if out, err := runSignCheck(t, "v0.4.1-rc.1", dist); err != nil {
		t.Fatalf("matching version refused: %v: %s", err, out)
	}
	for _, tc := range []struct{ version, want string }{
		{"v0.4.1.rc.1", notTag},
		{"v0.4.1-rc.1x", notTag},
		{"", notTag},
		{"v0.4.0-rc.1", "does not carry v0.4.0-rc.1"},
		{"v0.4.1", "does not carry v0.4.1"},
		{"v0.4.1-rc.2", "does not carry v0.4.1-rc.2"},
	} {
		out, err := runSignCheck(t, tc.version, dist)
		if err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("VERSION=%q: err %v, output %q; want a refusal saying %q", tc.version, err, out, tc.want)
		}
	}
	// a stamp that only starts with VERSION (rc.10 for rc.1) is not it
	if out, err := runSignCheck(t, "v0.4.1-rc.1", fakeDist(t, "v0.4.1-rc.10")); err == nil {
		t.Errorf("rc.10 binaries accepted as rc.1: %s", out)
	}
	// SHA256SUMS must describe the files present: a changed binary
	bad := fakeDist(t, "v0.4.1-rc.1")
	write(t, filepath.Join(bad, "foxess-local_linux_arm64"), "\x7fELF…v0.4.1-rc.1…changed")
	if out, err := runSignCheck(t, "v0.4.1-rc.1", bad); err == nil || !strings.Contains(out, "SHA256SUMS") {
		t.Errorf("changed binary: err %v, output %q", err, out)
	}
	// and a missing one
	gone := fakeDist(t, "v0.4.1-rc.1")
	if err := os.Remove(filepath.Join(gone, "foxess-local_linux_armv7")); err != nil {
		t.Fatal(err)
	}
	if out, err := runSignCheck(t, "v0.4.1-rc.1", gone); err == nil || !strings.Contains(out, "SHA256SUMS") {
		t.Errorf("missing binary: err %v, output %q", err, out)
	}
}
