package identity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGenerate: the hotspot name is foxess-<4 hex>, the host the same,
// and the password three groups of four from an alphabet without
// look-alikes (it is typed from a label when the QR code fails).
func TestGenerate(t *testing.T) {
	id, err := Generate(bytes.NewReader(bytes.Repeat([]byte{0x1a, 0x2b, 0x00, 0xff, 0x7f}, 20)))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^foxess-[0-9a-f]{4}$`).MatchString(id.SSID) || id.Host != id.SSID {
		t.Fatalf("name: %+v", id)
	}
	if !regexp.MustCompile(`^[` + alphabet + `]{4}-[` + alphabet + `]{4}-[` + alphabet + `]{4}$`).MatchString(id.Password) {
		t.Fatalf("password %q", id.Password)
	}
	if strings.ContainsAny(alphabet, "il1o0") {
		t.Fatalf("alphabet has look-alikes: %s", alphabet)
	}
	if _, err := Generate(bytes.NewReader(nil)); err == nil {
		t.Fatal("generated from an empty random source")
	}
}

// TestWriteRead: written root-only, read back the same; an existing
// file is never overwritten (the label on the stick names it).
func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "hotspot.env")
	id := Identity{SSID: "foxess-1a2b", Password: "abcd-efgh-jkmn", Host: "foxess-1a2b"}
	if err := Write(path, id); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
	got, err := Read(path)
	if err != nil || got != id {
		t.Fatalf("read %+v %v", got, err)
	}
	if err := Write(path, Identity{SSID: "foxess-ffff", Password: "abcd-efgh-jkmn", Host: "foxess-ffff"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote: %v", err)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{ //nolint:gosec // G101: test fixtures, not credentials
		"no host":        "SSID=foxess-1a2b\nPASSWORD=stick-password\n",
		"short password": "SSID=foxess-1a2b\nPASSWORD=short\nHOST=foxess-1a2b\n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := filepath.Join(dir, "good")
	if err := os.WriteFile(p, []byte("# from preparation\nSSID=foxess-1a2b\nPASSWORD=stick-password\nHOST=foxess-1a2b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := Read(p); err != nil || id.Password != "stick-password" {
		t.Fatalf("%+v %v", id, err)
	}
}
