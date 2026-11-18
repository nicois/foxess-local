package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyDir: a release directory verifies when SHA256SUMS carries a
// good signature and every listed file is present with its hash; a
// modified file, a missing file, or an unsigned list fail.
func TestVerifyDir(t *testing.T) {
	priv, id, pubText := testKey(t)
	pk, _ := ParsePublicKey(pubText)
	mk := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{"foxess-local_linux_amd64": "AMD", "installer.sh": "#!/bin/sh\n"}
		sums := ""
		for _, n := range []string{"foxess-local_linux_amd64", "installer.sh"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(files[n]), 0o600); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256([]byte(files[n]))
			sums += fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), n)
		}
		_ = os.WriteFile(filepath.Join(dir, SumsName), []byte(sums), 0o600)
		_ = os.WriteFile(filepath.Join(dir, SigName), []byte(sign(priv, id, "ED", []byte(sums), "file:SHA256SUMS")), 0o600)
		return dir
	}
	dir := mk(t)
	got, err := VerifyDir(pk, dir)
	if err != nil || len(got) != 2 {
		t.Fatalf("good release: %v %v", got, err)
	}
	dir = mk(t)
	_ = os.WriteFile(filepath.Join(dir, "installer.sh"), []byte("#!/bin/sh\nevil\n"), 0o600)
	if _, err := VerifyDir(pk, dir); !errors.Is(err, ErrChecksum) {
		t.Errorf("modified file: %v", err)
	}
	dir = mk(t)
	_ = os.Remove(filepath.Join(dir, "installer.sh"))
	if _, err := VerifyDir(pk, dir); err == nil {
		t.Error("missing file verified")
	}
	dir = mk(t)
	_ = os.WriteFile(filepath.Join(dir, SigName), []byte("junk"), 0o600)
	if _, err := VerifyDir(pk, dir); !errors.Is(err, ErrBadSignature) {
		t.Errorf("bad signature: %v", err)
	}
}
