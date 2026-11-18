package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGuard(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("sh", "upgrade-guard.sh")
	cmd.Env = append(os.Environ(), "BIN_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard: %v: %s", err, out)
	}
	return string(out)
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		return ""
	}
	return string(b)
}

// TestUpgradeGuard: with no pending upgrade the guard does nothing; with
// one, each start is counted; the fourth start restores the previous
// binary and records the failure (systemd then starts the old binary).
func TestUpgradeGuard(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "foxess-local")
	write(t, bin, "NEW")
	write(t, bin+".prev", "OLD")
	runGuard(t, dir)
	if read(t, bin) != "NEW" {
		t.Fatal("no pending upgrade, but the binary changed")
	}
	write(t, filepath.Join(dir, "upgrade-pending"), "from=v0.1.0\nto=v0.2.0\nattempts=0\n")
	for i := 1; i <= 3; i++ {
		runGuard(t, dir)
		if got := read(t, filepath.Join(dir, "upgrade-pending")); !strings.Contains(got, "attempts="+string(rune('0'+i))) {
			t.Fatalf("start %d: marker %q", i, got)
		}
		if read(t, bin) != "NEW" {
			t.Fatalf("start %d: rolled back too early", i)
		}
	}
	out := runGuard(t, dir)
	if read(t, bin) != "OLD" || read(t, filepath.Join(dir, "upgrade-pending")) != "" ||
		!strings.Contains(read(t, filepath.Join(dir, "upgrade-failed")), "to=v0.2.0") {
		t.Fatalf("fourth start: binary %q, failed %q", read(t, bin), read(t, filepath.Join(dir, "upgrade-failed")))
	}
	if !strings.Contains(out, "restored v0.1.0") {
		t.Fatalf("guard output %q", out)
	}
}

// TestReleaseKey: the embedded release key is empty (a source build) or
// a well-formed minisign public key.
func TestReleaseKey(t *testing.T) {
	k := strings.TrimSpace(string(ReleaseKey))
	if k == "" {
		return
	}
	if !strings.Contains(k, "\n") || !strings.HasPrefix(strings.TrimSpace(strings.Split(k, "\n")[1]), "RW") {
		t.Fatalf("release-key.pub is not a minisign public key")
	}
}

// TestUpgradeGuardKeepsOnline: the guard's rewrite keeps the app's
// online= flag (install review G2: the new binary must bring the
// inverter back before the upgrade is confirmed).
func TestUpgradeGuardKeepsOnline(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "foxess-local"), "NEW")
	write(t, filepath.Join(dir, "upgrade-pending"), "from=v0.1.0\nto=v0.2.0\nattempts=0\nonline=1\n")
	runGuard(t, dir)
	if got := read(t, filepath.Join(dir, "upgrade-pending")); !strings.Contains(got, "online=1") || !strings.Contains(got, "attempts=1") {
		t.Fatalf("marker after a start: %q", got)
	}
}

// TestUpgradeGuardSkipsEnvironmentalExit (install review G4): a start
// that ended because the inverter port was still held (exit=port_in_use,
// written by the app) is not counted against the new binary — up to a
// cap, so a binary that always claims it cannot dodge the rollback.
func TestUpgradeGuardSkipsEnvironmentalExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "foxess-local")
	p := filepath.Join(dir, "upgrade-pending")
	write(t, bin, "NEW")
	write(t, bin+".prev", "OLD")
	write(t, p, "from=v0.1.0\nto=v0.2.0\nattempts=1\nonline=1\nexit=port_in_use\n")
	out := runGuard(t, dir)
	got := read(t, p)
	if !strings.Contains(got, "attempts=1") || strings.Contains(got, "exit=") ||
		!strings.Contains(got, "uncounted=1") || !strings.Contains(got, "online=1") {
		t.Fatalf("after a port-in-use exit: marker %q", got)
	}
	if !strings.Contains(out, "not counted") {
		t.Fatalf("guard output %q", out)
	}
	// the cap: further claimed exits count once it is reached
	for range 5 {
		write(t, p, strings.Replace(read(t, p), "attempts=", "exit=port_in_use\nattempts=", 1))
		runGuard(t, dir)
	}
	if read(t, bin) != "OLD" {
		t.Fatalf("uncounted starts never capped: marker %q", read(t, p))
	}
}
