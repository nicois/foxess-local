package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/device"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
	devscan "foxesslocal/internal/scan"
	"foxesslocal/internal/store"
)

const (
	testConn = "1794820938317-1"
	invSN    = "TESTINVSN000001"
	logSN    = "TESTLOGSN000002"
	batSN1   = "TESTBATSN000003"
)

func snap01() []byte {
	p := make([]byte, 141)
	copy(p, invSN)
	copy(p[33:], "1.73")
	copy(p[93:], "KH10")
	return p
}

func snap60() []byte {
	p := make([]byte, 280)
	copy(p, invSN)
	copy(p[50:], batSN1)
	return p
}

// fixture: frames carrying the identifiers in several classes, an RTC
// read response, all on 2026-11-16.
func fixture() Input {
	t0 := time.Date(2026, 11, 16, 9, 30, 15, 0, time.UTC)
	reg := append([]byte{0x01, 0x00, 0x01, 0x11, 0x0F}, []byte(logSN)...)
	rtc := []byte{0x01, 0x03, 0x0C, 0x07, 0xEA, 0x00, 0x0B, 0x00, 0x10, 0x00, 0x14, 0x00, 0x24, 0x00, 0x13}
	keep := append([]byte("K12104"), []byte(invSN)...)
	fr := func(i int, class frame.Kind, p []byte) store.FrameRecord {
		return store.FrameRecord{
			ConnID: testConn, Seq: i, Dir: capture.DirC2S,
			TS: t0.Add(time.Duration(i) * time.Minute), Class: class, Plen: len(p), Payload: p, Decision: store.DecisionForwarded,
		}
	}
	return Input{
		Snapshot: device.Snapshot{SN: invSN, View: device.View{
			Model: "KH10", Firmware: identity.Firmware{Master: "1.73", Slave: "1.03", Manager: "1.71"},
			Report: compat.Report{Layout: "KH_PRE133", Family: "KH"},
		}},
		Frames: []store.FrameRecord{
			fr(0, "register", reg), fr(1, "snap01", snap01()), fr(2, "snap60", snap60()),
			fr(3, "keepalive", keep), fr(4, "cfg91", rtc),
		},
		Registers: map[int]uint16{30000: 'K', 31000: 1261},
		Version:   "test",
		Now:       t0.Add(time.Hour),
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = c
	}
}

// TestBundleDeidentified: no identifier survives anywhere in the
// archive, in ASCII or hex; payload lengths are kept (same-length
// pseudonyms); dates are shifted to 2000-01-01 with the time of day kept;
// the RTC date is zeroed; connection IDs are renumbered.
func TestBundleDeidentified(t *testing.T) {
	var buf bytes.Buffer
	m, err := Build(&buf, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if m.Redacted.Identifiers != 3 || m.Redacted.RTC != 1 {
		t.Fatalf("manifest redaction counts %+v", m.Redacted)
	}
	files := untar(t, buf.Bytes())
	for _, name := range []string{"manifest.json", "compatibility.json", "registers.csv", "frames.jsonl"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("missing %s (have %v)", name, files)
		}
	}
	if err := scan(files, []string{invSN, logSN, batSN1, "1794820938317"}); err != nil {
		t.Fatalf("identifier left: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(files["frames.jsonl"])), "\n")
	var f0, f4 struct {
		T       time.Time `json:"t"`
		Conn    string    `json:"conn"`
		Payload string    `json:"payload"`
	}
	_ = json.Unmarshal([]byte(lines[0]), &f0)
	_ = json.Unmarshal([]byte(lines[4]), &f4)
	if f0.T.Format("2006-01-02 15:04:05") != "2000-01-01 09:30:15" || f0.Conn != "conn-1" {
		t.Fatalf("frame 0: %+v", f0)
	}
	if p, _ := hex.DecodeString(f0.Payload); len(p) != 5+len(logSN) {
		t.Fatalf("register payload length changed: %d", len(p))
	}
	if p, _ := hex.DecodeString(f4.Payload); !bytes.Equal(p[3:9], make([]byte, 6)) {
		t.Fatalf("RTC date not zeroed: %x", p)
	}
}

// TestScanFailsClosed: an identifier left anywhere — ASCII or hex — aborts.
func TestScanFailsClosed(t *testing.T) {
	ids := []string{invSN}
	for _, c := range []string{"x" + invSN, "p=" + hex.EncodeToString([]byte(invSN))} {
		if err := scan(map[string][]byte{"f": []byte(c)}, ids); !errors.Is(err, ErrIdentifierLeft) {
			t.Errorf("%q: %v", c[:8], err)
		}
	}
	if err := scan(map[string][]byte{"f": []byte("clean")}, ids); err != nil {
		t.Fatal(err)
	}
}

// TestBundleRTCRegistersDateZeroed: the inverter clock is polled with the
// settings (0x9C40 ×6 = 40000..40005, year…second), so registers.csv
// would carry the real date; like the RTC read responses in the frame
// log, the date registers are zeroed and the time of day is kept.
func TestBundleRTCRegistersDateZeroed(t *testing.T) {
	in := fixture()
	in.Registers = map[int]uint16{31000: 1261, 40000: 2026, 40001: 11, 40002: 16, 40003: 9, 40004: 30, 40005: 15}
	var buf bytes.Buffer
	if _, err := Build(&buf, in); err != nil {
		t.Fatal(err)
	}
	csv := string(untar(t, buf.Bytes())["registers.csv"])
	for _, line := range []string{"40000,0\n", "40001,0\n", "40002,0\n", "40003,9\n", "40004,30\n", "40005,15\n", "31000,1261\n"} {
		if !strings.Contains(csv, line) {
			t.Errorf("registers.csv lacks %q:\n%s", line, csv)
		}
	}
	if strings.Contains(csv, ",2026") {
		t.Errorf("the year survived:\n%s", csv)
	}
}

// TestBundleReport (F8): a problem report adds report.json — the tester's
// description and the app's own facts, plus recent problem events — de-
// identified like the rest: serials pseudonymised (even one typed into
// the description), connection IDs renumbered, token names dropped, and
// times given as seconds before the report.
func TestBundleReport(t *testing.T) {
	in := fixture()
	in.Report = &Report{
		Description: "Readings stop after an hour. Inverter " + invSN + ".",
		Facts:       map[string]any{"forward_mode": "telemetry_only"},
	}
	in.Events = []store.AuditEvent{
		{TS: in.Now.Add(-10 * time.Minute), Kind: "ack-error", Detail: "1794820938317-1: echo with no in-flight command"},
		{TS: in.Now.Add(-5 * time.Minute), Kind: "command", Detail: "workmode 1 by phone"},
	}
	var buf bytes.Buffer
	if _, err := Build(&buf, in); err != nil {
		t.Fatal(err)
	}
	files := untar(t, buf.Bytes())
	raw, ok := files["report.json"]
	if !ok {
		t.Fatalf("no report.json (have %v)", files)
	}
	var rep struct {
		Description string         `json:"description"`
		Facts       map[string]any `json:"facts"`
		Events      []struct {
			BeforeS int    `json:"seconds_before_report"`
			Kind    string `json:"kind"`
			Detail  string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), invSN) || !strings.Contains(rep.Description, "Readings stop") {
		t.Fatalf("description: %q", rep.Description)
	}
	if rep.Facts["forward_mode"] != "telemetry_only" || len(rep.Events) != 2 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Events[0].BeforeS != 600 || strings.Contains(rep.Events[0].Detail, "1794820938317") {
		t.Errorf("event 0: %+v", rep.Events[0])
	}
	if strings.Contains(rep.Events[1].Detail, "phone") {
		t.Errorf("token name kept: %+v", rep.Events[1])
	}
}

// regsOf packs ASCII into big-endian registers (two characters each).
func regsOf(s string) []uint16 {
	if len(s)%2 == 1 {
		s += " "
	}
	out := make([]uint16, 0, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		out = append(out, uint16(s[i])<<8|uint16(s[i+1]))
	}
	return out
}

// TestBundleScan (F8): a device scan's results go into
// register-scan.jsonl as hex, de-identified: the known serials, and any
// other long alphanumeric run in the register data (a serial the app has
// never seen, on an untested model), are pseudonymised at the same
// length; a failed block keeps its error.
func TestBundleScan(t *testing.T) {
	in := fixture()
	in.Report = &Report{Description: "scan"}
	in.Scan = []devscan.Result{
		{Map: "identity", Fn: 3, Addr: 30000, Count: 8, Values: regsOf("KH10" + invSN[:12])},
		{Map: "H1", Fn: 4, Addr: 11000, Count: 8, Values: regsOf("XYZ9876543210QRS")},
		{Map: "H1", Fn: 4, Addr: 11100, Count: 4, Err: "modbus exception 2"},
	}
	var buf bytes.Buffer
	if _, err := Build(&buf, in); err != nil {
		t.Fatal(err)
	}
	raw := string(untar(t, buf.Bytes())["register-scan.jsonl"])
	if raw == "" {
		t.Fatal("no register-scan.jsonl")
	}
	for _, leak := range []string{"XYZ9876543210QRS", hex.EncodeToString([]byte("XYZ9876543210QRS"))} {
		if strings.Contains(raw, leak) {
			t.Fatalf("unknown serial survived as %q:\n%s", leak, raw)
		}
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var l1, l2 struct {
		Fn    int    `json:"fn"`
		Addr  int    `json:"addr"`
		Hex   string `json:"hex"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(lines[1]), &l1)
	_ = json.Unmarshal([]byte(lines[2]), &l2)
	if l1.Fn != 4 || l1.Addr != 11000 || len(l1.Hex) != 32 {
		t.Fatalf("line 1: %+v", l1)
	}
	if l2.Error == "" || l2.Hex != "" {
		t.Fatalf("line 2: %+v", l2)
	}
}

// envClock encodes t as a frame envelope clock (device[1:4] = unix>>8,
// fn = unix & 0xff: profile §3, the cloud's bootstrap responses).
func envClock(lead byte, t time.Time) ([4]byte, byte) {
	b := binary.BigEndian.AppendUint32(nil, uint32(t.Unix())) //nolint:gosec // G115: a 2026 timestamp fits.
	return [4]byte{lead, b[0], b[1], b[2]}, b[3]
}

// TestBundleEnvelopeClockShifted: a frame whose envelope carries a clock
// near its own timestamp (the cloud stamps its bootstrap responses with
// unix time) is shifted with the dates — the real date must not be
// recoverable from the header. An envelope that is not a nearby clock
// (the logger's millisecond uptime) is kept as it is.
func TestBundleEnvelopeClockShifted(t *testing.T) {
	in := fixture()
	ts := time.Date(2026, 11, 16, 9, 59, 11, 0, time.UTC)
	stamp := ts.Add(-11 * time.Second) // stamped by the cloud, arrived later
	dev, fn := envClock(0xaa, stamp)
	in.Frames = append(in.Frames, store.FrameRecord{
		ConnID: testConn, Seq: 9, Dir: capture.DirS2C, TS: ts, Class: frame.KindBootstrapResp,
		Dev: dev, Fn: fn, Plen: 5, Payload: []byte{1, 1, 1, 0, 0}, Decision: store.DecisionForwarded,
	})
	uptime := [4]byte{0x2a, 0x01, 0x54, 0x07}
	in.Frames = append(in.Frames, store.FrameRecord{
		ConnID: testConn, Seq: 10, Dir: capture.DirC2S, TS: ts, Class: frame.KindRegister,
		Dev: uptime, Fn: 0xf2, Plen: 1, Payload: []byte{0}, Decision: store.DecisionForwarded,
	})
	var buf bytes.Buffer
	if _, err := Build(&buf, in); err != nil {
		t.Fatal(err)
	}
	var lines []frameLine
	for l := range strings.SplitSeq(strings.TrimSpace(string(untar(t, buf.Bytes())["frames.jsonl"])), "\n") {
		var fl frameLine
		if err := json.Unmarshal([]byte(l), &fl); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fl)
	}
	shifted := lines[len(lines)-2]
	wantDev, wantFn := envClock(0xaa, epoch.Add(stamp.Sub(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))))
	if shifted.Dev != hex.EncodeToString(wantDev[:]) || shifted.Fn != wantFn {
		t.Errorf("envelope clock: dev %s fn %d, want %x fn %d", shifted.Dev, shifted.Fn, wantDev, wantFn)
	}
	if kept := lines[len(lines)-1]; kept.Dev != "2a015407" || kept.Fn != 0xf2 {
		t.Errorf("uptime envelope changed: dev %s fn %d", kept.Dev, kept.Fn)
	}
}

// TestBundleReportNetworkDetails: the frame service's event details keep
// their network error text ("reset by peer" is not a token name), LAN
// addresses become stable placeholders (the same address, the same
// placeholder), and public addresses (the cloud's) are kept.
func TestBundleReportNetworkDetails(t *testing.T) {
	in := fixture()
	in.Report = &Report{Description: "the gateway is 192.168.0.51", Facts: map[string]any{}}
	in.Events = []store.AuditEvent{
		{TS: in.Now.Add(-3 * time.Minute), Kind: "flow-close", Detail: "1794820938317-1: write tcp 192.168.0.51:38080->8.209.116.72:14431: write: broken pipe"},
		{TS: in.Now.Add(-2 * time.Minute), Kind: "flow-close", Detail: "1794820938317-2: read tcp 8.209.116.72:14431->192.168.0.83:62173: read: connection reset by peer"},
		{TS: in.Now.Add(-1 * time.Minute), Kind: "setting", Detail: "site.time_zone = UTC by phone"},
	}
	var buf bytes.Buffer
	if _, err := Build(&buf, in); err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Description string `json:"description"`
		Events      []struct {
			Detail string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(untar(t, buf.Bytes())["report.json"], &rep); err != nil {
		t.Fatal(err)
	}
	raw := rep.Description
	for _, e := range rep.Events {
		raw += "\n" + e.Detail
	}
	for _, leak := range []string{"192.168.0.51", "192.168.0.83", "phone"} {
		if strings.Contains(raw, leak) {
			t.Errorf("%q survived: %s", leak, raw)
		}
	}
	for _, keep := range []string{
		"write tcp lan-1:38080->8.209.116.72:14431: write: broken pipe",
		"read tcp 8.209.116.72:14431->lan-2:62173: read: connection reset by peer",
		"the gateway is lan-1",
		"site.time_zone = UTC by a token",
	} {
		if !strings.Contains(raw, keep) {
			t.Errorf("want %q in %s", keep, raw)
		}
	}
}

// TestBundleRetentionNoted: the manifest says how long frames are kept,
// so a reader knows an empty stretch may be retention, not silence.
func TestBundleRetentionNoted(t *testing.T) {
	in := fixture()
	in.RawHours = 72
	var buf bytes.Buffer
	m, err := Build(&buf, in)
	if err != nil {
		t.Fatal(err)
	}
	if m.FramesKeptHours != 72 {
		t.Errorf("frames_kept_hours = %d", m.FramesKeptHours)
	}
	if !strings.Contains(strings.Join(m.Notes, " "), "72 h after their connection closed") {
		t.Errorf("notes: %q", m.Notes)
	}
}
