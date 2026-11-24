package netmgr

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner answers nmcli calls from a script: the first rule whose
// prefix matches the joined arguments wins.
type fakeRunner struct {
	rules []rule
	calls []string
}

type rule struct {
	prefix string
	out    string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	for _, r := range f.rules {
		if strings.HasPrefix(call, r.prefix) {
			return []byte(r.out), r.err
		}
	}
	return nil, nil
}

func (f *fakeRunner) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

var errExit = errors.New("exit status 4")

const (
	testSSID = "home"
	usbMoved = "10.200.6.1/24"
	usbImage = "192.168.6.1/24" // the image's USB link
)

func newNM(t *testing.T, r *fakeRunner) (*NM, string) {
	t.Helper()
	dir := t.TempDir()
	return &NM{Run: r, Iface: "wlan0", KeyfileDir: dir}, dir
}

func TestSplitTerse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`home:70:WPA2:6`, []string{testSSID, "70", "WPA2", "6"}},
		{`a\:b:70::11`, []string{"a:b", "70", "", "11"}},
		{`back\\slash:1:--:1`, []string{`back\slash`, "1", "--", "1"}},
	} {
		got := splitTerse(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitTerse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScan(t *testing.T) {
	r := &fakeRunner{rules: []rule{{prefix: "nmcli -t -f SSID,SIGNAL,SECURITY,CHAN dev wifi list ifname wlan0 --rescan yes", out: "" +
		"home:40:WPA2:6\n" +
		"home:81:WPA2:6\n" + // a second access point of the same network
		":90:WPA2:1\n" + // hidden
		`cafe\:guest:55::11` + "\n" +
		"neighbour:20:WPA1 WPA2:1\n"}}}
	n, _ := newNM(t, r)
	got, err := n.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Network{
		{SSID: testSSID, Signal: 81, Secured: true},
		{SSID: "cafe:guest", Signal: 55, Secured: false},
		{SSID: "neighbour", Signal: 20, Secured: true},
	}
	if len(got) != len(want) {
		t.Fatalf("Scan = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Scan[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestJoinHome(t *testing.T) {
	r := &fakeRunner{}
	n, dir := newNM(t, r)
	psk := `p@ss word\with;odd chars`
	if err := n.JoinHome(context.Background(), "home net", psk); err != nil {
		t.Fatalf("JoinHome: %v", err)
	}
	try := filepath.Join(dir, tryID+".nmconnection")
	fi, err := os.Stat(try)
	if err != nil {
		t.Fatalf("trial keyfile: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("trial keyfile mode %v, want 0600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(try) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	kf := string(b)
	for _, want := range []string{"id=" + tryID, "autoconnect=false", "ssid=104;111;109;101;32;110;101;116;", `psk=p@ss word\\with;odd chars`} {
		if !strings.Contains(kf, want+"\n") {
			t.Errorf("trial keyfile lacks %q:\n%s", want, kf)
		}
	}
	// the trial profile becomes the home profile in place: no second
	// switch, no secrets on a command line
	want := []string{
		"nmcli con load " + try,
		"nmcli --wait 45 con up id " + tryID,
		"nmcli con delete id " + homeID,
		"nmcli con modify id " + tryID + " connection.id " + homeID + " connection.autoconnect yes",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range r.calls {
		if strings.Contains(c, "p@ss") {
			t.Fatalf("the password went on a command line: %q", c)
		}
	}
}

func TestJoinHomeFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		out  string
		want error
	}{
		{"wrong password", "Error: Connection activation failed: Secrets were required, but not provided", ErrWrongPassword},
		{"not found", "Error: Connection activation failed: The Wi-Fi network could not be found", ErrNotFound},
		{"other", "Error: Connection activation failed: IP configuration could not be reserved", ErrJoinFailed},
	} {
		t.Run(c.name, func(t *testing.T) { checkFailedJoin(t, c.out, c.want) })
	}
}

// checkFailedJoin: a join that fails with out maps to want, leaves the
// working profile alone, and removes the trial profile.
func checkFailedJoin(t *testing.T, out string, want error) {
	t.Helper()
	r := &fakeRunner{rules: []rule{{prefix: "nmcli --wait 45 con up id " + tryID, out: out, err: errExit}}}
	n, dir := newNM(t, r)
	home := filepath.Join(dir, homeID+".nmconnection")
	if err := os.WriteFile(home, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.JoinHome(context.Background(), testSSID, "a-good-length"); !errors.Is(err, want) {
		t.Fatalf("err %v, want %v", err, want)
	}
	b, err := os.ReadFile(home) //nolint:gosec // G304: the test's own temp dir
	if err != nil || string(b) != "previous\n" {
		t.Fatalf("a failed join touched the working profile: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, tryID+".nmconnection")); !os.IsNotExist(err) {
		t.Fatalf("the trial keyfile is still there: %v", err)
	}
	if !r.called("nmcli con delete id " + tryID) {
		t.Fatalf("the trial profile wasn't removed from NetworkManager: %q", r.calls)
	}
}

func TestJoinHomeValidates(t *testing.T) {
	for _, c := range []struct{ ssid, psk string }{
		{"", "a-good-length"},
		{strings.Repeat("x", 33), "a-good-length"},
		{testSSID, "short"},
		{testSSID, strings.Repeat("x", 64)},
		{testSSID, "bad\nnewline"},
	} {
		r := &fakeRunner{}
		n, _ := newNM(t, r)
		if err := n.JoinHome(context.Background(), c.ssid, c.psk); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q/%q: err %v, want ErrInvalid", c.ssid, c.psk, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("%q/%q: ran %q before validating", c.ssid, c.psk, r.calls)
		}
	}
	// an open network: no password at all
	r := &fakeRunner{}
	n, dir := newNM(t, r)
	if err := n.JoinHome(context.Background(), "open", ""); err != nil {
		t.Fatalf("open network: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, tryID+".nmconnection")) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "wifi-security") {
		t.Fatalf("an open network got a security section:\n%s", b)
	}
}

func TestHotspot(t *testing.T) {
	r := &fakeRunner{}
	n, dir := newNM(t, r)
	if err := n.HotspotUp(context.Background(), "foxess-1a2b", "stick-password"); err != nil {
		t.Fatalf("HotspotUp: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, hotspotID+".nmconnection")) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode=ap", "autoconnect=false", "address1=" + HotspotAddr.String() + "/24", "method=manual", "proto=rsn"} {
		if !strings.Contains(string(b), want+"\n") {
			t.Errorf("hotspot keyfile lacks %q:\n%s", want, b)
		}
	}
	if !r.called("nmcli --wait 30 con up id " + hotspotID) {
		t.Errorf("not brought up: %q", r.calls)
	}
	if err := n.HotspotDown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.called("nmcli --wait 30 con down id " + hotspotID) {
		t.Errorf("not brought down: %q", r.calls)
	}
}

func TestActive(t *testing.T) {
	for _, c := range []struct {
		out  string
		role Role
		addr string
	}{
		{"GENERAL.CONNECTION:" + hotspotID + "\nIP4.ADDRESS[1]:10.42.0.1/24\n", RoleHotspot, "10.42.0.1/24"},
		{"GENERAL.CONNECTION:" + homeID + "\nIP4.ADDRESS[1]:192.0.2.40/24\n", RoleHome, "192.0.2.40/24"},
		{"GENERAL.CONNECTION:wifi-client\nIP4.ADDRESS[1]:198.51.100.7/16\n", RoleHome, "198.51.100.7/16"},
		{"GENERAL.CONNECTION:" + tryID + "\n", RoleJoining, ""},
		{"GENERAL.CONNECTION:\n", RoleNone, ""},
	} {
		r := &fakeRunner{rules: []rule{{prefix: "nmcli -t -f GENERAL.CONNECTION,IP4.ADDRESS dev show wlan0", out: c.out}}}
		n, _ := newNM(t, r)
		st, err := n.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Role != c.role || (c.addr != "" && st.Addr.String() != c.addr) || (c.addr == "" && st.Addr.IsValid()) {
			t.Errorf("%q: %+v, want %v %s", c.out, st, c.role, c.addr)
		}
	}
}

func TestUSBSubnet(t *testing.T) {
	for _, c := range []struct{ home, usb, want string }{
		{"192.168.0.0/16", usbImage, usbMoved},
		{"192.0.2.0/24", usbImage, ""}, // no overlap: leave it
		{"10.0.0.0/8", usbImage, ""},
		{"10.0.0.0/8", usbMoved, "172.31.6.1/24"},
		{"172.16.0.0/12", "172.31.6.1/24", usbMoved},
	} {
		got, ok := USBSubnet(netip.MustParsePrefix(c.home), netip.MustParsePrefix(c.usb))
		if c.want == "" {
			if ok {
				t.Errorf("%s/%s: moved to %s, want left alone", c.home, c.usb, got)
			}
			continue
		}
		if !ok || got.String() != c.want {
			t.Errorf("%s/%s: %s %v, want %s", c.home, c.usb, got, ok, c.want)
		}
	}
}

// TestEscapeValue: checked against NetworkManager 1.42.4 on the stick
// (2026-11-24): this keyfile value reads back as " p@ss word\with;odd chars".
func TestEscapeValue(t *testing.T) {
	if got := escapeValue(` p@ss word\with;odd chars`); got != `\sp@ss word\\with;odd chars` {
		t.Fatalf("escapeValue = %q", got)
	}
}

func TestHomeKnown(t *testing.T) {
	for _, c := range []struct {
		out  string
		want bool
	}{
		{"lo:loopback\nusb:802-3-ethernet\nhotspot:802-11-wireless\nlte:gsm\n", false},
		{"usb:802-3-ethernet\nwifi-client:802-11-wireless\nhotspot:802-11-wireless\n", true}, // the image's, before seal
		{homeID + ":802-11-wireless\n" + hotspotID + ":802-11-wireless\n", true},
		{tryID + ":802-11-wireless\n" + hotspotID + ":802-11-wireless\n", false},
	} {
		r := &fakeRunner{rules: []rule{{prefix: "nmcli -t -f NAME,TYPE con show", out: c.out}}}
		n, _ := newNM(t, r)
		got, err := n.HomeKnown(context.Background())
		if err != nil || got != c.want {
			t.Errorf("%q: %v %v, want %v", c.out, got, err, c.want)
		}
	}
}
