package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCmd(t, "version")
	if code != 0 || strings.TrimSpace(out) != version {
		t.Fatalf("version: code %d, out %q", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nonsense"}} {
		code, _, errOut := runCmd(t, args...)
		if code != exitUsage || !strings.Contains(errOut, "usage: foxess-wizard") {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
}

// TestIMEICheckNoDevice: a missing modem device is a clear failure, not
// a hang or a panic.
func TestIMEICheckNoDevice(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "wwan0qmi0")
	code, _, errOut := runCmd(t, "imei-check", "-dev", dev)
	if code != exitFailed || !strings.Contains(errOut, "no such file") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}
