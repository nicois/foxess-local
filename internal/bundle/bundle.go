// Package bundle exports a de-identified diagnostic bundle (design
// D-022): what an owner of a not-yet-verified inverter can review and
// share so the model can be supported. Nothing is uploaded; the owner
// downloads the archive and decides.
//
// De-identification: every device identifier the app knows (inverter
// SN, datalogger SN, battery module SNs) is replaced in every payload by
// a same-length pseudonym (offsets survive); RTC read responses have
// their date zeroed; connection IDs are renumbered; dates are shifted to
// 2000-01-01 keeping the time of day, frame envelope clocks with them;
// LAN addresses in the report's text become placeholders. Then the whole archive is scanned
// for each identifier in ASCII and hex: any hit aborts the export.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"foxesslocal/internal/device"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
	devscan "foxesslocal/internal/scan"
	"foxesslocal/internal/store"
)

// ErrIdentifierLeft is the fail-closed scan finding an identifier.
var ErrIdentifierLeft = errors.New("bundle: a device identifier survived redaction; export aborted")

// Input is what goes into a bundle.
type Input struct {
	Snapshot  device.Snapshot
	Frames    []store.FrameRecord
	Registers map[int]uint16
	Version   string
	Now       time.Time
	// Report, when set, makes the bundle a problem report (F8):
	// report.json carries it with Events (recent problem events).
	Report *Report
	Events []store.AuditEvent
	// Scan is the last read-only device scan (register-scan.jsonl).
	Scan []devscan.Result
	// RawHours is the frame retention (retention.raw_hours): frames are
	// kept this long after their connection closed (0: not stated).
	RawHours int
}

// scanLine is one register-scan.jsonl record: the block and its register
// bytes (big-endian) as hex, or the read's error.
type scanLine struct {
	Map   string `json:"map"`
	Fn    int    `json:"fn"`
	Addr  int    `json:"addr"`
	Count int    `json:"count"`
	Hex   string `json:"hex,omitempty"`
	Error string `json:"error,omitempty"`
}

// Register byte layout: two bytes, big-endian.
const (
	regBytesPer = 2
	regHiShift  = 8
)

// regBytes is a block's register values as big-endian bytes.
func regBytes(vals []uint16) []byte {
	out := make([]byte, 0, regBytesPer*len(vals))
	for _, v := range vals {
		out = append(out, byte(v>>regHiShift), byte(v)) //nolint:gosec // G115: the shift keeps a byte
	}
	return out
}

// addReportFiles adds a problem report's files: report.json (with a
// report) and register-scan.jsonl (with a scan).
func addReportFiles(files map[string][]byte, in Input, ids []string, conns map[string]string) error {
	var err error
	if in.Report != nil {
		if files["report.json"], err = reportJSON(in, ids, conns); err != nil {
			return err
		}
	}
	if len(in.Scan) > 0 {
		if files["register-scan.jsonl"], err = scanJSONL(in.Scan, ids); err != nil {
			return err
		}
	}
	return nil
}

// scanJSONL writes the scan, each block's bytes pseudonymised first.
func scanJSONL(results []devscan.Result, ids []string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, r := range results {
		l := scanLine{Map: r.Map, Fn: r.Fn, Addr: r.Addr, Count: r.Count, Error: r.Err}
		if r.Err == "" {
			l.Hex = hex.EncodeToString(redact(regBytes(r.Values), ids))
		}
		if err := enc.Encode(l); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// Report is a tester's problem report: their description and the app's
// own facts (version, device, settings, setup checks — whatever the host
// passes; JSON-encodable).
type Report struct {
	Description string         `json:"description"`
	Facts       map[string]any `json:"facts"`
}

// reportEvent is one recent event, timed relative to the report.
type reportEvent struct {
	BeforeS int    `json:"seconds_before_report"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
}

// connIDPattern matches a connection ID (ms timestamp + sequence); a
// token name follows " by " at the end of an audit detail.
var (
	connIDPattern = regexp.MustCompile(`\b\d{13}-\d+\b`)
	byTokenSuffix = regexp.MustCompile(` by [^ ]+$`)
	ipv4Pattern   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// networkKinds are the frame service's audit kinds: their details end in
// network error text ("connection reset by peer"), never a token name.
var networkKinds = map[string]bool{
	"flow-open": true, "flow-close": true, "serve": true, "tls": true, "reconnect": true,
	"ack-error": true, "inject-error": true, "cloud-drop": true,
}

// lanNames replaces LAN addresses (private, loopback, link-local IPv4)
// with stable placeholders (lan-1, lan-2… by first appearance); public
// addresses, such as the cloud's, are kept.
type lanNames map[netip.Addr]string

func (l lanNames) redact(s string) string {
	return ipv4Pattern.ReplaceAllStringFunc(s, func(m string) string {
		a, err := netip.ParseAddr(m)
		if err != nil || (!a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast()) {
			return m
		}
		n, ok := l[a]
		if !ok {
			n = fmt.Sprintf("lan-%d", len(l)+1)
			l[a] = n
		}
		return n
	})
}

// reportJSON builds report.json: identifiers pseudonymised everywhere
// (the description included), connection IDs renumbered with the frame
// log's numbering, token names dropped, times relative to the report.
func reportJSON(in Input, ids []string, conns map[string]string) ([]byte, error) {
	out := struct {
		Report
		Events []reportEvent `json:"events"`
	}{Report: *in.Report, Events: []reportEvent{}}
	lan := lanNames{}
	for _, e := range in.Events {
		d := connIDPattern.ReplaceAllStringFunc(e.Detail, func(id string) string {
			if c, ok := conns[id]; ok {
				return c
			}
			c := fmt.Sprintf("conn-%d", len(conns)+1)
			conns[id] = c
			return c
		})
		if !networkKinds[e.Kind] {
			d = byTokenSuffix.ReplaceAllString(d, " by a token")
		}
		d = lan.redact(d)
		out.Events = append(out.Events, reportEvent{
			BeforeS: int(in.Now.Sub(e.TS).Round(time.Second) / time.Second),
			Kind:    e.Kind, Detail: d,
		})
	}
	out.Description = lan.redact(out.Description)
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return nil, err
	}
	return redact(b, ids), nil
}

// Manifest describes the bundle (manifest.json).
type Manifest struct {
	Format     int       `json:"format"`
	AppVersion string    `json:"app_version"`
	Created    time.Time `json:"created"` // shifted like every timestamp
	Frames     int       `json:"frames"`
	// FramesKeptHours is the frame retention: no frame of a connection
	// that closed longer ago than this is in the bundle.
	FramesKeptHours int            `json:"frames_kept_hours,omitempty"`
	Identity        manifestDevice `json:"identity"`
	Redacted        Redaction      `json:"redacted"`
	Notes           []string       `json:"notes"`
}

type manifestDevice struct {
	Model           string            `json:"model"`
	Firmware        identity.Firmware `json:"firmware"`
	Layout          string            `json:"layout"`
	Family          string            `json:"family"`
	VerifiedProfile string            `json:"verified_profile,omitempty"`
}

// Redaction counts what was removed.
type Redaction struct {
	Identifiers int `json:"identifiers"`
	RTC         int `json:"rtc_responses"`
	Conns       int `json:"connections_renumbered"`
}

// frameLine is one frames.jsonl record.
type frameLine struct {
	T        time.Time      `json:"t"`
	Conn     string         `json:"conn"`
	Seq      int            `json:"seq"`
	Dir      string         `json:"dir"`
	Class    frame.Kind     `json:"class"`
	Fn       byte           `json:"fn"`
	Dev      string         `json:"dev"`
	Decision store.Decision `json:"decision"`
	Payload  string         `json:"payload"`
}

// epoch is where shifted dates start.
var epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// minIdentifierLen guards against redacting short, common byte strings;
// registerSNMin is the shortest ASCII run taken from the registration
// frame as the datalogger SN (the reference logger SN is 15 chars).
const (
	minIdentifierLen = 6
	registerSNMin    = 10
	fileMode         = 0o644
)

// Build writes the bundle (tar.gz) to w.
func Build(w io.Writer, in Input) (Manifest, error) {
	ids := identifiers(in)
	shift := dayShift(in)
	m := Manifest{
		Format: 1, AppVersion: in.Version, Created: in.Now.Add(-shift), Frames: len(in.Frames),
		Identity: manifestDevice{
			Model: in.Snapshot.Model, Firmware: in.Snapshot.Firmware,
			Layout: in.Snapshot.Report.Layout, Family: in.Snapshot.Report.Family,
			VerifiedProfile: in.Snapshot.Report.Profile,
		},
		Notes: []string{
			"Device serial numbers are replaced by same-length pseudonyms; dates are shifted to 2000-01-01 (time of day kept, UTC).",
			"Payloads are the frame payloads (no framing / CRC); 32-bit values are as on the wire.",
		},
	}
	if in.RawHours > 0 {
		m.FramesKeptHours = in.RawHours
		m.Notes = append(m.Notes, fmt.Sprintf("Frames are kept for %d h after their connection closed (retention.raw_hours): "+
			"an older connection has no frames here, though its events may be listed.", in.RawHours))
	}
	m.Redacted.Identifiers = len(ids)

	conns := map[string]string{}
	var frames bytes.Buffer
	enc := json.NewEncoder(&frames)
	for _, f := range in.Frames {
		p := redact(f.Payload, ids)
		if isRTCResponse(f.Class, p) {
			p = zeroRTC(p)
			m.Redacted.RTC++
		}
		c, ok := conns[f.ConnID]
		if !ok {
			c = fmt.Sprintf("conn-%d", len(conns)+1)
			conns[f.ConnID] = c
		}
		dev, fn := shiftEnvelopeClock(f, shift)
		if err := enc.Encode(frameLine{
			T: f.TS.Add(-shift), Conn: c, Seq: f.Seq, Dir: f.Dir.String(), Class: f.Class,
			Fn: fn, Dev: hex.EncodeToString(dev[:]), Decision: f.Decision,
			Payload: hex.EncodeToString(p),
		}); err != nil {
			return Manifest{}, err
		}
	}
	m.Redacted.Conns = len(conns)

	files := map[string][]byte{"frames.jsonl": frames.Bytes()}
	var err error
	if files["manifest.json"], err = json.MarshalIndent(m, "", " "); err != nil {
		return Manifest{}, err
	}
	if files["compatibility.json"], err = json.MarshalIndent(in.Snapshot.Report, "", " "); err != nil {
		return Manifest{}, err
	}
	files["registers.csv"] = registersCSV(in.Registers)
	if err := addReportFiles(files, in, ids, conns); err != nil {
		return Manifest{}, err
	}
	if err := scan(files, ids); err != nil {
		return Manifest{}, err
	}
	return m, writeTarGz(w, files, in.Now.Add(-shift))
}

// identifiers collects every device identifier the data carries: the
// inverter SN, the battery module SNs (device-info / module snapshots),
// and the datalogger SN (the registration frame's ASCII field).
func identifiers(in Input) []string {
	set := map[string]bool{}
	add := func(s string) {
		if len(s) >= minIdentifierLen {
			set[s] = true
		}
	}
	add(in.Snapshot.SN)
	// a scan of an untested model may hold serials the snapshots never
	// showed: every long alphanumeric run in its register data counts
	for _, r := range in.Scan {
		for _, run := range asciiRuns(regBytes(r.Values), registerSNMin) {
			add(run)
		}
	}
	for _, f := range in.Frames {
		// frame.Kind is open-ended (SnapKind): the known classes, then the default
		//exhaustive:ignore
		switch f.Class {
		case frame.KindSnap01:
			add(identity.FromSnapshots(f.Payload, nil).SN)
		case frame.KindSnap60:
			for _, sn := range identity.FromSnapshots(nil, f.Payload).BatterySNs {
				add(sn)
			}
		case frame.KindRegister:
			for _, run := range asciiRuns(f.Payload, registerSNMin) {
				add(run)
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out) // pseudonyms are stable across exports of the same data
	return out
}

// asciiRuns returns the alphanumeric runs of at least n bytes.
func asciiRuns(p []byte, n int) []string {
	var out []string
	start := -1
	for i := 0; i <= len(p); i++ {
		alnum := i < len(p) && (p[i] >= '0' && p[i] <= '9' || p[i] >= 'A' && p[i] <= 'Z' || p[i] >= 'a' && p[i] <= 'z')
		switch {
		case alnum && start < 0:
			start = i
		case !alnum && start >= 0:
			if i-start >= n {
				out = append(out, string(p[start:i]))
			}
			start = -1
		}
	}
	return out
}

// pseudonym is a same-length stand-in ("REDACTED1XXXXXX").
func pseudonym(i, n int) string {
	s := fmt.Sprintf("REDACTED%d", i+1)
	if len(s) > n {
		return strings.Repeat("X", n)
	}
	return s + strings.Repeat("X", n-len(s))
}

func redact(p []byte, ids []string) []byte {
	out := append([]byte(nil), p...)
	for i, id := range ids {
		out = bytes.ReplaceAll(out, []byte(id), []byte(pseudonym(i, len(id))))
	}
	return out
}

// An RTC read response: fn03 with 12 data bytes (0x9C40 ×6, the only
// six-register read the app and the cloud make).
const (
	rtcByteCount = 12
	rtcDateLo    = 3 // year
	rtcDateHi    = 9 // through day
)

func isRTCResponse(class frame.Kind, p []byte) bool {
	return class == frame.KindCfg91 && len(p) == 3+rtcByteCount && p[1] == 0x03 && p[2] == rtcByteCount
}

func zeroRTC(p []byte) []byte {
	for i := rtcDateLo; i < rtcDateHi; i++ {
		p[i] = 0
	}
	return p
}

// envelopeClockSpan is how near a frame's timestamp an envelope value
// must be to count as a clock: the cloud stamps UTC, the inverter its
// local time (up to ±14 h), so a day covers both.
const envelopeClockSpan = 24 * time.Hour

// shiftEnvelopeClock moves a frame envelope's clock with the dates. The
// envelope's device[1:4] + fn are a sender's 32-bit seconds counter on
// the cloud's bootstrap responses (profile §3) and on the inverter's own
// session frames: left alone they give the real date back. Only a value
// within envelopeClockSpan of the frame's timestamp is treated as a
// clock (the logger's millisecond uptime is not one).
func shiftEnvelopeClock(f store.FrameRecord, shift time.Duration) ([4]byte, byte) {
	u := int64(f.Dev[1])<<24 | int64(f.Dev[2])<<16 | int64(f.Dev[3])<<8 | int64(f.Fn)
	if d := time.Duration(f.TS.Unix()-u) * time.Second; d > envelopeClockSpan || d < -envelopeClockSpan {
		return f.Dev, f.Fn
	}
	b := binary.BigEndian.AppendUint32(nil, uint32(u-int64(shift/time.Second))) //nolint:gosec // G115: a shifted date (2000) fits 32 bits.
	return [4]byte{f.Dev[0], b[0], b[1], b[2]}, b[3]
}

// dayShift moves the earliest frame's date to the epoch, whole days only.
func dayShift(in Input) time.Duration {
	first := in.Now
	for _, f := range in.Frames {
		if f.TS.Before(first) {
			first = f.TS
		}
	}
	day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)
	return day.Sub(epoch)
}

func registersCSV(regs map[int]uint16) []byte {
	addrs := make([]int, 0, len(regs))
	for a := range regs {
		addrs = append(addrs, a)
	}
	sort.Ints(addrs)
	var b strings.Builder
	b.WriteString("addr,value\n")
	for _, a := range addrs {
		v := regs[a]
		if a >= rtcRegFirst && a <= rtcRegDateLast {
			v = 0 // the inverter clock's date (year, month, day): like the RTC frames
		}
		fmt.Fprintf(&b, "%d,%d\n", a, v)
	}
	return []byte(b.String())
}

// The inverter clock registers (0x9C40 ×6: year, month, day, hour,
// minute, second — polled with the settings): the date is zeroed in
// registers.csv, the time of day kept.
const (
	rtcRegFirst    = 0x9C40
	rtcRegDateLast = rtcRegFirst + 2
)

// scan is the fail-closed check: every identifier, in ASCII and in both
// hex cases, against every file.
func scan(files map[string][]byte, ids []string) error {
	for name, c := range files {
		for _, id := range ids {
			h := hex.EncodeToString([]byte(id))
			for _, form := range []string{id, h, strings.ToUpper(h)} {
				if bytes.Contains(c, []byte(form)) {
					return fmt.Errorf("%w (in %s)", ErrIdentifierLeft, name)
				}
			}
		}
	}
	return nil
}

func writeTarGz(w io.Writer, files map[string][]byte, mtime time.Time) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: fileMode, Size: int64(len(files[n])), ModTime: mtime}); err != nil {
			return err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}
