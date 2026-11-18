package update

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VerifyDir checks a release directory: SHA256SUMS against its minisign
// signature, then every listed file against its hash. It returns the
// verified file names. Used before publishing a release and for manual
// installs (`foxess-local verify-release DIR`).
func VerifyDir(pk PublicKey, dir string) ([]string, error) {
	sums, err := os.ReadFile(filepath.Join(dir, SumsName)) //nolint:gosec // the release dir the operator named
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, SigName)) //nolint:gosec // the release dir the operator named
	if err != nil {
		return nil, err
	}
	if err := pk.Verify(sums, sig); err != nil {
		return nil, err
	}
	var ok []string
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		if name != filepath.Base(name) {
			return ok, fmt.Errorf("update: %s lists a path, not a file name: %q", SumsName, name)
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a name listed in the signed SHA256SUMS
		if err != nil {
			return ok, err
		}
		if h := sha256.Sum256(b); hex.EncodeToString(h[:]) != strings.ToLower(f[0]) {
			return ok, fmt.Errorf("%w: %s", ErrChecksum, name)
		}
		ok = append(ok, name)
	}
	return ok, sc.Err()
}
