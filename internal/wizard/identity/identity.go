// Package identity is an OpenStick appliance's own name and password
// (design D-031): the hotspot SSID, its WPA2 password (also the wizard's
// sign-in password), and the .local host name. Preparation generates
// them once and prints them on the stick's label; they never change
// afterwards.
package identity

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"foxesslocal/internal/wizard/netmgr"
)

// Path is where the wizard keeps the identity (root 0600).
const Path = "/etc/foxess-wizard/hotspot.env"

// alphabet is the password's: lower case and digits without the
// look-alikes i, l, o, 0 and 1 (it is typed from a label when the QR
// code fails).
const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"

const (
	groups    = 3 // of groupSize characters, joined by "-"
	groupSize = 4
	nameBytes = 2 // foxess-<4 hex>
	fileMode  = 0o600
	dirMode   = 0o700
)

// Identity is the stick's hotspot and name.
type Identity struct{ SSID, Password, Host string }

// Generate makes a new identity from rnd (crypto/rand.Reader).
func Generate(rnd io.Reader) (Identity, error) {
	var name [nameBytes]byte
	if _, err := io.ReadFull(rnd, name[:]); err != nil {
		return Identity{}, fmt.Errorf("identity: %w", err)
	}
	ssid := fmt.Sprintf("foxess-%x", name)
	var b strings.Builder
	for i := range groups * groupSize {
		if i > 0 && i%groupSize == 0 {
			b.WriteByte('-')
		}
		c, err := pick(rnd)
		if err != nil {
			return Identity{}, err
		}
		b.WriteByte(c)
	}
	return Identity{SSID: ssid, Password: b.String(), Host: ssid}, nil
}

// pick draws one alphabet character without bias (rejection sampling).
func pick(rnd io.Reader) (byte, error) {
	limit := byte(256 / len(alphabet) * len(alphabet))
	var c [1]byte
	for {
		if _, err := io.ReadFull(rnd, c[:]); err != nil {
			return 0, fmt.Errorf("identity: %w", err)
		}
		if c[0] < limit {
			return alphabet[int(c[0])%len(alphabet)], nil
		}
	}
}

// Write stores id at path (root 0600, its directory 0700). It never
// replaces an existing file: the label on the stick names its contents.
func Write(path string, id Identity) error {
	if err := netmgr.Validate(id.SSID, id.Password); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode) //nolint:gosec // G304: the fixed path, or a test's
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "# the stick's hotspot and name (provision-openstick.sh --appliance);\n"+
		"# printed on its label: do not change\nSSID=%s\nPASSWORD=%s\nHOST=%s\n", id.SSID, id.Password, id.Host)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// Read loads and checks the identity at path.
func Read(path string) (Identity, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the fixed path, or a test's
	if err != nil {
		return Identity{}, err
	}
	defer func() { _ = f.Close() }()
	var id Identity
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		switch k {
		case "SSID":
			id.SSID = v
		case "PASSWORD":
			id.Password = v
		case "HOST":
			id.Host = v
		}
	}
	if err := sc.Err(); err != nil {
		return Identity{}, err
	}
	if id.SSID == "" || id.Password == "" || id.Host == "" {
		return Identity{}, fmt.Errorf("%s: SSID, PASSWORD and HOST are all needed", path)
	}
	if err := netmgr.Validate(id.SSID, id.Password); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", path, err)
	}
	return id, nil
}
