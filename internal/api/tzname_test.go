package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIANAZone (foxess-control local backend, 2026-11-19): the host's
// IANA zone name, so the integration can spot a host / Home Assistant
// zone mismatch — TZ first, then /etc/timezone, then the /etc/localtime
// link; a name the time package cannot load is not reported.
func TestIANAZone(t *testing.T) {
	dir := t.TempDir()
	tzfile := filepath.Join(dir, "timezone")
	localtime := filepath.Join(dir, "localtime")
	if err := os.Symlink("/usr/share/zoneinfo/Australia/Sydney", localtime); err != nil {
		t.Fatal(err)
	}
	src := zoneSources{tzfile: tzfile, localtime: localtime}
	if got := ianaZone(src, ":Europe/Berlin"); got == nil || *got != "Europe/Berlin" {
		t.Fatalf("TZ: %v", got)
	}
	if got := ianaZone(src, ""); got == nil || *got != "Australia/Sydney" {
		t.Fatalf("localtime link: %v", got)
	}
	if err := os.WriteFile(tzfile, []byte("America/New_York\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ianaZone(src, ""); got == nil || *got != "America/New_York" {
		t.Fatalf("/etc/timezone: %v", got)
	}
	if got := ianaZone(zoneSources{tzfile: "/nonexistent", localtime: "/nonexistent"}, "Not/AZone"); got != nil {
		t.Fatalf("an unloadable name: %v", *got)
	}
}
