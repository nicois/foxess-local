package deploy

import (
	"os"
	"strings"
	"testing"
)

// TestInstallDocKey (owner, 2026-11-18): INSTALL.md's verification step
// carries the release key itself — the one built into every binary
// (deploy/release-key.pub) — so a first install can check a download
// against a key that did not come with it, and a key rotation cannot
// leave the docs behind.
func TestInstallDocKey(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(ReleaseKey)), "\n")
	if len(lines) != 2 {
		t.Fatalf("release-key.pub: %d lines, want the comment and the key", len(lines))
	}
	key := strings.TrimSpace(lines[1])
	doc, err := os.ReadFile("../INSTALL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "minisign -Vm SHA256SUMS -P "+key) {
		t.Fatalf("INSTALL.md does not verify with the release key %s", key)
	}
	id := strings.TrimPrefix(strings.TrimSpace(lines[0]), "untrusted comment: minisign public key ")
	if !strings.Contains(string(doc), id) {
		t.Fatalf("INSTALL.md does not name the key ID %s", id)
	}
}
