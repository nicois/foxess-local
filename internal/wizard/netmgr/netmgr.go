// Package netmgr is the wizard's view of the stick's one radio, through
// NetworkManager (design D-031): scan, join the home network, raise and
// drop the hotspot, and report which role is active.
//
// Secrets never go on a command line, where every local account could
// read them in the process list. The wizard writes root-only keyfiles
// and has NetworkManager load them. A new home network is tried under a
// separate profile, so a wrong password never replaces credentials
// that work.
package netmgr

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Profile ids the wizard owns. The image's own profiles are left alone.
const (
	homeID    = "foxess-wizard-home"
	tryID     = "foxess-wizard-try"
	hotspotID = "foxess-wizard-hotspot"
)

// KeyfileDir is where NetworkManager keeps system profiles.
const KeyfileDir = "/etc/NetworkManager/system-connections"

// HotspotAddr is the stick's address on its hotspot; the wizard's
// dnsmasq answers every name with it.
var HotspotAddr = netip.MustParseAddr("10.42.0.1")

const hotspotBits = 24

// keyfileMode keeps profiles (they hold passwords) root-only.
const keyfileMode = 0o600

// Waits passed to nmcli: joining includes the 6.5 s a wrong password
// takes to report (spike S2) and a slow DHCP server.
const (
	joinWait    = "45"
	hotspotWait = "30"
)

// WPA2 passphrase and SSID limits.
const (
	minPSK  = 8
	maxPSK  = 63
	maxSSID = 32
)

// Join failures the wizard explains differently.
var (
	ErrInvalid       = errors.New("netmgr: invalid network name or password")
	ErrWrongPassword = errors.New("netmgr: the network refused the password")
	ErrNotFound      = errors.New("netmgr: the network is not in range")
	ErrJoinFailed    = errors.New("netmgr: could not join the network")
)

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Exec runs commands for real, with C-locale messages (the failure
// matching reads them).
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: this package's fixed nmcli invocations
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

// NM drives NetworkManager for one Wi-Fi interface.
type NM struct {
	Run        Runner
	Iface      string // wlan0
	KeyfileDir string // KeyfileDir, or a test's temp dir
}

// Network is one scanned network, the strongest of its access points.
type Network struct {
	SSID    string
	Signal  int // 0–100
	Secured bool
}

// Scan lists the networks in range, strongest first, without hidden
// ones. It needs client mode: in AP mode the radio can't scan (S2).
func (n *NM) Scan(ctx context.Context) ([]Network, error) {
	out, err := n.Run.Run(ctx, "nmcli", "-t", "-f", "SSID,SIGNAL,SECURITY,CHAN", "dev", "wifi", "list", "ifname", n.Iface, "--rescan", "yes")
	if err != nil {
		return nil, fmt.Errorf("netmgr: scan: %w: %s", err, strings.TrimSpace(string(out)))
	}
	best := map[string]Network{}
	for line := range strings.Lines(string(out)) {
		f := splitTerse(strings.TrimRight(line, "\n"))
		if len(f) < 3 || f[0] == "" {
			continue
		}
		sig, _ := strconv.Atoi(f[1])
		nw := Network{SSID: f[0], Signal: sig, Secured: f[2] != "" && f[2] != "--"}
		if old, ok := best[nw.SSID]; !ok || nw.Signal > old.Signal {
			best[nw.SSID] = nw
		}
	}
	list := slices.Collect(func(yield func(Network) bool) {
		for _, nw := range best {
			if !yield(nw) {
				return
			}
		}
	})
	slices.SortFunc(list, func(a, b Network) int {
		return cmp.Or(cmp.Compare(b.Signal, a.Signal), cmp.Compare(a.SSID, b.SSID))
	})
	return list, nil
}

// JoinHome tries ssid with psk (empty: an open network). On success the
// trial profile becomes the home profile, replacing any older one; on
// failure the older one is untouched.
func (n *NM) JoinHome(ctx context.Context, ssid, psk string) error {
	if err := validate(ssid, psk); err != nil {
		return err
	}
	kf, err := homeKeyfile(tryID, n.Iface, ssid, psk)
	if err != nil {
		return err
	}
	path, err := n.writeKeyfile(tryID, kf)
	if err != nil {
		return err
	}
	if out, lerr := n.Run.Run(ctx, "nmcli", "con", "load", path); lerr != nil {
		n.dropTrial(ctx, path)
		return fmt.Errorf("netmgr: load profile: %w: %s", lerr, strings.TrimSpace(string(out)))
	}
	if out, uerr := n.Run.Run(ctx, "nmcli", "--wait", joinWait, "con", "up", "id", tryID); uerr != nil {
		n.dropTrial(ctx, path)
		return joinError(uerr, string(out))
	}
	// the old home profile is inactive now; a missing one is fine
	_, _ = n.Run.Run(ctx, "nmcli", "con", "delete", "id", homeID)
	if out, merr := n.Run.Run(ctx, "nmcli", "con", "modify", "id", tryID, "connection.id", homeID, "connection.autoconnect", "yes"); merr != nil {
		return fmt.Errorf("netmgr: keep the profile: %w: %s", merr, strings.TrimSpace(string(out)))
	}
	return nil
}

func (n *NM) dropTrial(ctx context.Context, path string) {
	_, _ = n.Run.Run(ctx, "nmcli", "con", "delete", "id", tryID)
	_ = os.Remove(path)
}

func joinError(err error, out string) error {
	switch {
	case strings.Contains(out, "Secrets were required"):
		return fmt.Errorf("%w (%w)", ErrWrongPassword, err)
	case strings.Contains(out, "could not be found"), strings.Contains(out, "No network with SSID"):
		return fmt.Errorf("%w (%w)", ErrNotFound, err)
	default:
		return fmt.Errorf("%w: %s (%w)", ErrJoinFailed, strings.TrimSpace(out), err)
	}
}

// HotspotUp raises the wizard's hotspot: WPA2, manual addressing on
// HotspotAddr (the wizard's own dnsmasq serves it; spike S1).
func (n *NM) HotspotUp(ctx context.Context, ssid, psk string) error {
	if err := validate(ssid, psk); err != nil || psk == "" {
		return cmp.Or(err, ErrInvalid)
	}
	kf, err := hotspotKeyfile(n.Iface, ssid, psk)
	if err != nil {
		return err
	}
	path, err := n.writeKeyfile(hotspotID, kf)
	if err != nil {
		return err
	}
	if out, lerr := n.Run.Run(ctx, "nmcli", "con", "load", path); lerr != nil {
		return fmt.Errorf("netmgr: load hotspot: %w: %s", lerr, strings.TrimSpace(string(out)))
	}
	if out, uerr := n.Run.Run(ctx, "nmcli", "--wait", hotspotWait, "con", "up", "id", hotspotID); uerr != nil {
		return fmt.Errorf("netmgr: hotspot up: %w: %s", uerr, strings.TrimSpace(string(out)))
	}
	return nil
}

// HotspotDown drops the hotspot; NetworkManager then autoconnects the
// home profile if it's in range.
func (n *NM) HotspotDown(ctx context.Context) error {
	if out, err := n.Run.Run(ctx, "nmcli", "--wait", hotspotWait, "con", "down", "id", hotspotID); err != nil {
		return fmt.Errorf("netmgr: hotspot down: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Role is what the radio is doing.
type Role int

// The roles.
const (
	RoleNone    Role = iota
	RoleHome         // a client connection other than the wizard's trial or hotspot
	RoleJoining      // the trial profile
	RoleHotspot
)

// Status is the radio's role and its IPv4 address with prefix.
type Status struct {
	Role Role
	Addr netip.Prefix // invalid when there is none
}

// Active reports the interface's current role and address.
func (n *NM) Active(ctx context.Context) (Status, error) {
	out, err := n.Run.Run(ctx, "nmcli", "-t", "-f", "GENERAL.CONNECTION,IP4.ADDRESS", "dev", "show", n.Iface)
	if err != nil {
		return Status{}, fmt.Errorf("netmgr: status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var st Status
	for line := range strings.Lines(string(out)) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), ":")
		if !ok {
			continue
		}
		switch {
		case k == "GENERAL.CONNECTION":
			st.Role = roleOf(v)
		case strings.HasPrefix(k, "IP4.ADDRESS") && !st.Addr.IsValid():
			if p, perr := netip.ParsePrefix(v); perr == nil {
				st.Addr = p
			}
		}
	}
	return st, nil
}

func roleOf(conn string) Role {
	switch conn {
	case "", "--":
		return RoleNone
	case hotspotID:
		return RoleHotspot
	case tryID:
		return RoleJoining
	default:
		return RoleHome
	}
}

// USBSubnet picks a new subnet for the USB link when the home network
// contains it (its more specific route would hide those home hosts).
// It returns false when the USB link can stay where it is.
func USBSubnet(home, usb netip.Prefix) (netip.Prefix, bool) {
	if !home.Overlaps(usb) {
		return netip.Prefix{}, false
	}
	for _, c := range []string{"10.200.6.1/24", "172.31.6.1/24", "192.168.250.1/24"} {
		p := netip.MustParsePrefix(c)
		if !home.Overlaps(p) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

func validate(ssid, psk string) error {
	if ssid == "" || len(ssid) > maxSSID {
		return ErrInvalid
	}
	if psk == "" {
		return nil
	}
	if len(psk) < minPSK || len(psk) > maxPSK {
		return ErrInvalid
	}
	for _, r := range psk {
		if r < ' ' || r > '~' {
			return ErrInvalid
		}
	}
	return nil
}

func (n *NM) writeKeyfile(id, content string) (string, error) {
	path := filepath.Join(n.KeyfileDir, id+".nmconnection")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), keyfileMode); err != nil {
		return "", fmt.Errorf("netmgr: write profile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("netmgr: write profile: %w", err)
	}
	return path, nil
}

func homeKeyfile(id, iface, ssid, psk string) (string, error) {
	uuid, err := newUUID()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[connection]\nid=%s\nuuid=%s\ntype=wifi\ninterface-name=%s\nautoconnect=false\n\n", id, uuid, iface)
	fmt.Fprintf(&b, "[wifi]\nmode=infrastructure\nssid=%s\n\n", ssidBytes(ssid))
	if psk != "" {
		fmt.Fprintf(&b, "[wifi-security]\nkey-mgmt=wpa-psk\npsk=%s\n\n", escapeValue(psk))
	}
	b.WriteString("[ipv4]\nmethod=auto\n\n[ipv6]\nmethod=auto\n")
	return b.String(), nil
}

func hotspotKeyfile(iface, ssid, psk string) (string, error) {
	uuid, err := newUUID()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[connection]\nid=%s\nuuid=%s\ntype=wifi\ninterface-name=%s\nautoconnect=false\n\n", hotspotID, uuid, iface)
	fmt.Fprintf(&b, "[wifi]\nmode=ap\nband=bg\nssid=%s\n\n", ssidBytes(ssid))
	fmt.Fprintf(&b, "[wifi-security]\nkey-mgmt=wpa-psk\nproto=rsn\npairwise=ccmp\ngroup=ccmp\npsk=%s\n\n", escapeValue(psk))
	fmt.Fprintf(&b, "[ipv4]\nmethod=manual\naddress1=%s/%d\n\n[ipv6]\nmethod=disabled\n", HotspotAddr, hotspotBits)
	return b.String(), nil
}

// ssidBytes writes an SSID as the keyfile's byte list, which carries
// any byte (spaces, ';', non-ASCII) without escaping rules.
func ssidBytes(ssid string) string {
	var b strings.Builder
	for i := range len(ssid) {
		b.WriteString(strconv.Itoa(int(ssid[i])))
		b.WriteByte(';')
	}
	return b.String()
}

// escapeValue escapes a keyfile string value (GLib key-file rules):
// backslash, and a leading space. The PSK is printable ASCII, so no
// control characters reach here.
func escapeValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	if strings.HasPrefix(s, " ") {
		s = `\s` + s[1:]
	}
	return s
}

func newUUID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("netmgr: uuid: %w", err)
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

// splitTerse splits one line of `nmcli -t` output: fields separated by
// ':', with ':' and '\' escaped by a backslash.
func splitTerse(line string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(fields, cur.String())
}
