package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestReadIdentity(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("# from preparation\nSSID=foxess-1a2b\nPASSWORD=stick-password\nHOST=foxess-1a2b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := readIdentity(good)
	if err != nil || id != (identity{SSID: "foxess-1a2b", Password: "stick-password", Host: "foxess-1a2b"}) {
		t.Fatalf("identity %+v %v", id, err)
	}
	for name, body := range map[string]string{ //nolint:gosec // G101: test fixtures, not credentials
		"no host":        "SSID=foxess-1a2b\nPASSWORD=stick-password\n",
		"short password": "SSID=foxess-1a2b\nPASSWORD=short\nHOST=foxess-1a2b\n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readIdentity(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDemoSnapshots(t *testing.T) {
	for _, s := range []string{"unconfigured", "joining", "normal", "degraded", "home_lost", "hotspot", "awaiting_inverter"} {
		if got := demoSnapshot(s, "").Decision.State.String(); got != s {
			t.Errorf("demo %s shows %s", s, got)
		}
	}
}
