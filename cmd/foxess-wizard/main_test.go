package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foxesslocal/internal/wizard/identity"
)

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := invoke(t, "version")
	if code != 0 || strings.TrimSpace(out) != version {
		t.Fatalf("version: code %d, out %q", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nonsense"}} {
		code, _, errOut := invoke(t, args...)
		if code != exitUsage || !strings.Contains(errOut, "usage: foxess-wizard") {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
}

// TestIMEICheckNoDevice: a missing modem device is a clear failure, not
// a hang or a panic.
func TestIMEICheckNoDevice(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "wwan0qmi0")
	code, _, errOut := invoke(t, "imei-check", "-dev", dev)
	if code != exitFailed || !strings.Contains(errOut, "no such file") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestDemoSnapshots(t *testing.T) {
	for _, s := range []string{"unconfigured", "joining", "normal", "degraded", "home_lost", "hotspot", "awaiting_inverter"} {
		if got := demoSnapshot(s, "").Decision.State.String(); got != s {
			t.Errorf("demo %s shows %s", s, got)
		}
	}
}

// TestInitAndLabel: init creates the identity once and keeps it on a
// re-run (the label names it); label writes the text and the PNG.
func TestInitAndLabel(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "hotspot.env")
	code, out, errOut := invoke(t, "init", "-config", cfg)
	if code != 0 || !strings.Contains(out, "created") {
		t.Fatalf("init: %d %q %q", code, out, errOut)
	}
	first, err := identity.Read(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, _ := invoke(t, "init", "-config", cfg); code != 0 || !strings.Contains(out, "kept "+first.SSID) {
		t.Fatalf("second init: %d %q", code, out)
	}
	if again, _ := identity.Read(cfg); again != first {
		t.Fatalf("identity changed: %+v -> %+v", first, again)
	}
	labels := filepath.Join(dir, "labels")
	code, out, errOut = invoke(t, "label", "-config", cfg, "-out", labels)
	if code != 0 || !strings.Contains(out, first.Password) {
		t.Fatalf("label: %d %q %q", code, out, errOut)
	}
	for _, ext := range []string{".txt", ".png"} {
		if fi, err := os.Stat(filepath.Join(labels, first.SSID+"-label"+ext)); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", ext, err)
		}
	}
}
