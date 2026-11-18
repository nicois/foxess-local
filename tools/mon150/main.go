// Command mon150 tails (or replays) a prox inverter capture and decodes it.
//
// Live mode (no -from/-to): tails c2s.bin and s2c.bin of a prox connection
// (one directory per cloud connection under /var/lib/prox/conns/<date>/),
// by default following the newest capture. c2s: full 150B telemetry decode
// plus one line per other frame; s2c: every frame rendered as a classified
// Modbus line (cloud read/write requests, inverter responses, 57B serial
// queries), so app-triggered downlink commands can be read live.
//
// Replay mode (-from/-to): re-analyse history. The capture files carry no
// per-byte timestamps, so the timeline is reconstructed: t0/t_end =
// conn_open/conn_close from the connection's meta.jsonl (c2s mtime when the
// close ts is absent), c2s clock = the 5 s tele heartbeat (end-anchored,
// drift distributed; with fewer than two tele frames, the steady 30 s
// keepalive cadence anchors, else byte-fraction, marked '~'). s2c frames
// take their time from the per-write bytes_s2c meta events when the capture
// carries them (exact), else from the inverter's c2s response (fn10/fn06
// write-ack by signature, read responses FIFO per fn), else a byte-fraction
// estimate, marked '~'. Frames whose offset falls in an s2c_drop range are
// flagged DROPPED (captured but never delivered to the inverter). The
// summary adds each type's median inter-event interval (whole session). Output is the merged, time-ordered timeline
// restricted to the range, so each pass can be re-run with a tighter range
// as the interpretation improves.
//
// Usage:
//
//	mon150 [-dur 60s] [-file .../c2s.bin] [-s2c .../s2c.bin] [-hexother]
//	HA_TOKEN=... mon150 -from 12:20 -to 12:28 [-file .../c2s.bin]
//
// -from/-to accept HH:MM[:SS] (local, matched within the connection's day;
// a value before t0 wraps to the next day) or an integer = seconds since
// connection open.
//
// Decodes (KH10, see docs/recon/protocol-profile.md): 150 B power telemetry
// (fields + identity checks + HA cross-check), 135 B K12104 keepalive,
// 16 B RSSI, t0 registration (logger SN), 6 B Modbus write/read echoes with
// register names, device-info snapshot series (141 B SN+fw+model, 280 B
// battery module SN list, 425 B inverter + 626 B battery status snapshots
// as hex — layout not yet decoded), 40 B SN frame, cfg91 K12104 config / Modbus read
// responses; downlink: Modbus reads/writes with validated register names and
// a 48010 time-slot decode (windows, fd soc/pwr, min_soc, default mode).
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	protocol "foxesslocal/internal/frame"
)

var (
	filePath = flag.String("file", "", "c2s capture (default: newest under /var/lib/prox/conns)")
	dur      = flag.Duration("dur", 60*time.Second, "how long live mode runs")
	haURL    = flag.String("ha", "", "Home Assistant base URL for the value cross-check, e.g. http://homeassistant.local:8123 (empty disables)")
	hexOther = flag.Bool("hexother", false, "hex-dump non-150B c2s frames too")
	pollIv   = flag.Duration("poll", 150*time.Millisecond, "file poll interval")
	s2cPath  = flag.String("s2c", "", "s2c capture (default: s2c.bin next to -file)")
	from     = flag.String("from", "", "replay mode: range start, HH:MM[:SS] or seconds since conn_open")
	to       = flag.String("to", "", "replay mode: range end, same formats (default: end of file)")
)

// ---------- framing ----------

type frame struct {
	off      int64
	device   [4]byte
	fn       byte
	payload  []byte
	detected time.Time
}

func crc16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// scan parses complete frames from buf; returns the frames and how many
// leading bytes are fully consumed (up to the end of the last complete frame).
func scan(buf []byte, baseOff int64) ([]frame, int) {
	var out []frame
	consumed := 0
	for i := range buf {
		if i+13 > len(buf) {
			break
		}
		start := string(buf[i : i+2])
		if start != "\x7e\x7e" && start != "\x7f\x7f" {
			continue
		}
		plen := int(binary.BigEndian.Uint16(buf[i+7 : i+9]))
		if i+13+plen > len(buf) {
			break // incomplete frame
		}
		body := buf[i+2 : i+9+plen]
		crc := binary.LittleEndian.Uint16(buf[i+9+plen : i+11+plen])
		wantEnd := "\xe7\xe7"
		if start == "\x7f\x7f" {
			wantEnd = "\xf7\xf7"
		}
		if crc16(body) == crc && string(buf[i+11+plen:i+13+plen]) == wantEnd {
			var dev [4]byte
			copy(dev[:], buf[i+2:i+6])
			out = append(out, frame{
				off:     baseOff + int64(i),
				device:  dev,
				fn:      buf[i+6],
				payload: append([]byte(nil), buf[i+9:i+9+plen]...),
			})
			consumed = i + 13 + plen
			i = consumed - 1
			continue
		}
	}
	return out, consumed
}

func hexdump(p []byte) string {
	var s strings.Builder
	for a := 0; a < len(p); a += 16 {
		b := p[a:min(a+16, len(p))]
		s.WriteString(fmt.Sprintf("    %3d: %s\n", a, hexSpaced(b)))
	}
	return s.String()
}

func hexSpaced(b []byte) string {
	var s strings.Builder
	for i, c := range b {
		if i > 0 {
			s.WriteString(" ")
		}
		s.WriteString(fmt.Sprintf("%02x", c))
	}
	return s.String()
}

// ---------- HA ----------

var shortNames = map[string]string{
	"sensor.foxess_solarleistung":            "sol",
	"sensor.foxess_entladeleistung":          "dsg",
	"sensor.foxess_ladeleistung":             "chg",
	"sensor.foxess_hausverbrauch":            "haus",
	"sensor.foxess_netzbezug":                "imp",
	"sensor.foxess_netzeinspeisung":          "exp",
	"sensor.foxess_netzzahlerleistung":       "meter",
	"sensor.foxess_erzeugung":                "erz",
	"sensor.foxess_netzspannung":             "V",
	"sensor.foxess_netzstrom":                "A",
	"sensor.foxess_batterie_soc":             "soc",
	"sensor.foxess_batterietemperatur":       "battT",
	"sensor.foxess_bms_battery_temperature":  "bmsT",
	"sensor.foxess_wechselrichtertemperatur": "invT",
	"sensor.foxess_umgebungstemperatur":      "ambT",
	"sensor.foxess_einspeiseenergie":         "expWh",
	"sensor.foxess_entladeenergie":           "dsgWh",
	"sensor.foxess_hausverbrauchsenergie":    "hausWh",
	"sensor.foxess_solarenergie":             "solarWh",
	"sensor.foxess_netzbezugsenergie":        "impWh",
	"sensor.foxess_ladeenergie":              "chgWh",
	"sensor.foxess_discharge_export_limit":   "limit",
}

type haPoller struct {
	mu      sync.Mutex
	vals    map[string]string
	fetched time.Time
}

func startHA(url string) *haPoller {
	token := os.Getenv("HA_TOKEN")
	if url == "" || token == "" {
		return nil
	}
	p := &haPoller{vals: map[string]string{}}
	client := &http.Client{Timeout: 2 * time.Second}
	fetch := func() {
		req, _ := http.NewRequest(http.MethodGet, url+"/api/states", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var sts []struct {
			EntityID string `json:"entity_id"`
			State    string `json:"state"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&sts); err != nil {
			return
		}
		m := map[string]string{}
		for _, s := range sts {
			if _, ok := shortNames[s.EntityID]; ok {
				m[shortNames[s.EntityID]] = s.State
			}
		}
		p.mu.Lock()
		p.vals = m
		p.fetched = time.Now()
		p.mu.Unlock()
	}
	go func() {
		fetch()
		t := time.NewTicker(3 * time.Second)
		for range t.C {
			fetch()
		}
	}()
	return p
}

func (p *haPoller) line() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.vals) == 0 {
		return ""
	}
	age := time.Since(p.fetched).Seconds()
	order := []string{
		"sol", "dsg", "chg", "haus", "imp", "exp", "meter", "erz",
		"V", "A", "soc", "battT", "bmsT", "invT", "ambT",
	}
	var s strings.Builder
	for _, k := range order {
		if v, ok := p.vals[k]; ok {
			s.WriteString(fmt.Sprintf("%s=%s ", k, v))
		}
	}
	return fmt.Sprintf("HA(%.1fs): %s", age, s.String())
}

// nums returns the last parsed HA values as floats plus the fetch age in
// seconds; nil/0 when HA is disabled or nothing fetched yet.
func (p *haPoller) nums() (map[string]float64, float64) {
	if p == nil {
		return nil, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]float64{}
	for k, v := range p.vals {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m[k] = f
		}
	}
	return m, time.Since(p.fetched).Seconds()
}

// xha correlates the frame's decoded power values with the latest HA push
// and prints signed deltas (frame - HA, W; HA power sensors are kW). Zero
// deltas mean the frame instant aligns with the HA push; persistent
// nonzero deltas expose push lag or a mis-decoded field.
func xha(sol, grid, haus, batt, soc int, ha *haPoller) {
	n, age := ha.nums()
	if len(n) == 0 {
		return
	}
	var s strings.Builder
	if v, ok := n["sol"]; ok {
		s.WriteString(fmt.Sprintf(" sol %+dW", sol-int(v*1000+0.5)))
	}
	if v, ok := n["haus"]; ok {
		s.WriteString(fmt.Sprintf(" haus %+dW", haus-int(v*1000+0.5)))
	}
	if v, ok := n["soc"]; ok {
		s.WriteString(fmt.Sprintf(" soc %+.1f", float64(soc)-v))
	}
	battHA, hasBatt := 0.0, false
	if v, ok := n["chg"]; ok && v > 0 {
		battHA, hasBatt = v*1000, true
	} else if v, ok := n["dsg"]; ok && v > 0 {
		battHA, hasBatt = -v*1000, true
	}
	if hasBatt {
		s.WriteString(fmt.Sprintf(" batt %+dW", batt-int(battHA+0.5)))
	}
	gridHA, hasGrid := 0.0, false
	if v, ok := n["imp"]; ok && v > 0 {
		gridHA, hasGrid = v*1000, true
	} else if v, ok := n["exp"]; ok && v > 0 {
		gridHA, hasGrid = -v*1000, true
	}
	if hasGrid {
		s.WriteString(fmt.Sprintf(" grid %+dW", grid-int(gridHA+0.5)))
	}
	if s.Len() > 0 {
		fmt.Printf("  XHA(%.1fs):%s (frame-HA; 0 => aligned)\n", age, s.String())
	}
}

// ---------- c2s display ----------

func classify(f frame) string {
	p := f.payload
	switch {
	case len(p) == 150:
		return "tele"
	case len(p) == 135 && f.device[0] == 0x06:
		return "keepalive" // "K12104"+SN+zeros — periodic carrier
	case len(p) == 16:
		return "rssi" // 4 B head + JSON {"RSSI":N}
	case len(p) == 40:
		return "cfg40"
	case len(p) == 23 && p[0] == 0x01 && p[1] == 0x00:
		return "register" // logger/gateway SN registration at t0
	case len(p) == 6 && p[0] == 0x01 && (p[1] == 0x06 || p[1] == 0x10):
		return "mbecho" // c2s echo of an applied Modbus command
	case f.device[0] == 0x91:
		// K12104 config read responses and Modbus fn03 read responses
		// share device 0x91 and the 01 03 head; not separable by shape
		return fmt.Sprintf("cfg91_%dB", len(p))
	case len(p) == 141 || len(p) == 425 || len(p) == 202 ||
		len(p) == 280 || len(p) == 626 || len(p) == 165:
		// chunk selector is the device lead byte, not payload[0]
		return fmt.Sprintf("snap%02x", f.device[0])
	case len(p) == 451 && f.device[0] == 0x37:
		return "wrapper" // fn=64 push mode: 13 B prefix + a complete 438 B inner frame
	default:
		return fmt.Sprintf("other_%dB", len(p))
	}
}

// printC2S renders one non-telemetry uplink frame with its known decode;
// shapes without a known layout get a header line (hexdump with -hexother).
func printC2S(tag, kind string, f frame, ha *haPoller) {
	p := f.payload
	line := fmt.Sprintf("%s c2s --- %s off=%d dev=%x fn=%02x", tag, kind, f.off, f.device, f.fn)
	cstr := func(a, b int) string {
		return strings.TrimRight(string(p[a:b]), "\x00 ")
	}
	switch kind {
	case "keepalive":
		fmt.Printf("%s  SN=%s (K12104 carrier, presence only)\n", line, cstr(6, 21))
	case "rssi":
		fmt.Printf("%s  %s\n", line, strings.Trim(string(p[4:]), "{}"))
	case "register":
		fmt.Printf("%s  logger/gateway SN=%s (len=%d)\n", line, cstr(5, 20), p[4])
	case "mbecho":
		addr := u16BE(p, 2)
		n := ""
		if rn := protocol.RegName(addr); rn != "" {
			n = "  [" + rn + "]"
		}
		if p[1] == 0x06 {
			fmt.Printf("%s  echo fn06 write @%d (0x%04X) = %d%s\n", line, addr, addr, u16BE(p, 4), n)
		} else {
			fmt.Printf("%s  echo fn10 read @%d (0x%04X) x%d%s\n", line, addr, addr, u16BE(p, 4), n)
		}
	case "snap01": // 141 B device ID
		fmt.Printf("%s  devinfo: SN=%s fw=%s/%s/%s/%s model=%q\n", line, cstr(0, 15),
			cstr(33, 37), cstr(39, 43), cstr(45, 49), cstr(51, 55), cstr(93, 100))
	case "snap60": // 280 B battery module list
		sns := make([]string, 0, 10)
		for k := range 10 {
			if s := cstr(50+21*k, 65+21*k); s != "" {
				sns = append(sns, s)
			}
		}
		fmt.Printf("%s  devinfo: inv SN=%s + %d module SNs: %s\n", line, cstr(0, 15), len(sns), strings.Join(sns, " "))
	case "cfg40":
		fmt.Printf("%s  SN=%s count=%d\n", line, cstr(0, 15), u16BE(p, 32))
	case "snap02", "snap61": // 425 B inverter / 626 B battery status snapshot; layout not yet decoded
		fmt.Println(line)
		fmt.Print(hexdump(p))
		if ha != nil {
			if l := ha.line(); l != "" {
				fmt.Println("  " + l)
			}
		}
	default:
		fmt.Println(line)
		if *hexOther {
			fmt.Print(hexdump(p))
		}
	}
}

func u16BE(p []byte, off int) int {
	return int(binary.BigEndian.Uint16(p[off : off+2]))
}

func diff150(prev, cur []byte) string {
	var s string
	for i := range prev {
		if prev[i] != cur[i] {
			var ctx string
			if i+2 <= len(cur) {
				ctx = fmt.Sprintf(" Δu16=%+d", int16(u16BE(cur, i))-int16(u16BE(prev, i)))
			}
			s += fmt.Sprintf("  [%d] %02x->%02x%s\n", i, prev[i], cur[i], ctx)
		}
	}
	if s == "" {
		s = "  (no changes)\n"
	}
	return s
}

func print150(f frame, prev []byte, ha *haPoller) {
	p := f.payload
	fmt.Printf("\n=== tele 150B off=%d dev=%x t=%s\n", f.off, f.device,
		f.detected.Format("15:04:05.000"))
	fmt.Print(hexdump(p))
	// Power block: four signed 32-bit BE fields (W). Battery +charge/-discharge;
	// grid +import/-export at the panel; acport +inverter-out/-in at the inverter
	// AC side. PV bypasses the AC stage, so identities (exact at frame instant):
	//   grid = haus + batt - sol ;  acport = sol - batt
	s32 := func(i int) int { return int(int32(binary.BigEndian.Uint32(p[i : i+4]))) }
	sol, grid, haus := s32(35), s32(39), s32(43)
	ac, batt := s32(59), s32(67)
	fmt.Printf("  KNOWN: sol=%d W  grid=%+d W  haus=%d W (dup=%d)  acport=%+d W  batt=%+d W  soc=%d%%\n",
		sol, grid, haus, u16BE(p, 65), ac, batt, p[79])
	fmt.Printf("  IDNT: grid-(haus+batt-sol)=%+d W  acport-(sol-batt)=%+d W  (both 0 => self-consistent)\n",
		grid-haus-batt+sol, ac-sol+batt)
	xha(sol, grid, haus, batt, int(p[79]), ha)
	fmt.Printf("  FLAGS: [72]=%02x (charge state: 02 idle / 04 charging)  [73:75]=%d (bit8 set while charging)  [76:78]=%d  [83]=%02x  [97]=%02x\n",
		p[72], u16BE(p, 73), u16BE(p, 76), p[83], p[97])
	if prev != nil {
		fmt.Printf("  DIFF vs previous:\n%s", diff150(prev, p))
	}
	if l := ha.line(); l != "" {
		fmt.Println("  " + l)
	}
}

// ---------- s2c (downlink) display ----------

// nameS2C is the short count key for a downlink frame.
func nameS2C(fr frame) string {
	p := fr.payload
	if len(p) >= 2 && p[0] == 0x01 {
		resp := fr.device[0]&0x80 != 0
		switch p[1] {
		case 0x03, 0x04:
			if resp {
				return "rresp"
			}
			return "read"
		case 0x06, 0x10:
			if resp {
				return "wack"
			}
			return "write"
		}
	}
	if len(p) == 57 {
		return "query"
	}
	return "other"
}

// s2cSig returns the matching signature of a downlink request: write
// requests are answered in c2s by an ack echoing (fn, addr[, cnt]); read
// requests by a read response, matched FIFO per fn.
func s2cSig(fr frame) (fn byte, addr, cnt int, ok bool) {
	p := fr.payload
	if fr.device[0]&0x80 != 0 || len(p) < 6 || p[0] != 0x01 {
		return 0, 0, 0, false
	}
	switch p[1] {
	case 0x03, 0x04:
		if len(p) == 6 {
			return p[1], u16BE(p, 2), u16BE(p, 4), true
		}
	case 0x06:
		if len(p) == 6 {
			return p[1], u16BE(p, 2), -1, true
		}
	case 0x10:
		if len(p) >= 7 {
			return p[1], u16BE(p, 2), u16BE(p, 4), true
		}
	}
	return 0, 0, 0, false
}

// lastPush tracks the latest fn10 write per base address so consecutive
// read-modify-write pushes can be diffed (the scheduler push protocol).
var lastPush = map[int][]int{}

// pushDiff prints the registers that changed vs the prior write to the same
// base address; the first write to a base in this run is the baseline.
func pushDiff(addr int, p []byte, cnt int) {
	r := func(i int) int { return u16BE(p, 7+2*i) }
	cur := make([]int, cnt)
	for i := range cnt {
		cur[i] = r(i)
	}
	if prev, ok := lastPush[addr]; ok && len(prev) == cnt {
		var d []string
		for i := range cur {
			if cur[i] != prev[i] {
				d = append(d, fmt.Sprintf("r%d %d→%d (0x%04X→0x%04X)", i, prev[i], cur[i], prev[i], cur[i]))
			}
		}
		if len(d) == 0 {
			fmt.Printf("       Δ %d: (no change vs prior write)\n", addr)
		} else {
			fmt.Printf("       Δ %d: %s\n", addr, strings.Join(d, ", "))
		}
	}
	lastPush[addr] = cur
}

// printSlotTable renders the eight time groups of a ×80 block per the
// official Time Period Table (FoxESS Modbus spec V1.02 Table 3-11: per group
// en, start, end, workmode, maxsoc:minsoc, fdsoc, fdpwr, res, res, res) and
// flags any non-zero value in the reserved tail registers.
func printSlotTable(addr int, p []byte, cnt int) {
	r := func(i int) int { return u16BE(p, 7+2*i) }
	hhmm := func(i int) string { return fmt.Sprintf("%02d:%02d", r(i)>>8, r(i)&0xFF) }
	wm := map[int]string{1: "SU", 2: "FE", 3: "BC", 4: "PS", 5: "ST", 6: "FC", 7: "FD"}
	parts := make([]string, 0, 8)
	for k := range 8 {
		b := 10 * k
		if r(b) == 0 {
			parts = append(parts, fmt.Sprintf("s%d=off", k+1))
			continue
		}
		m, ok := wm[r(b+3)]
		if !ok {
			m = fmt.Sprintf("wm%d?", r(b+3))
		}
		parts = append(parts, fmt.Sprintf("s%d %s %s-%s %d:%d fd=%d%%/%dW", k+1, m, hhmm(b+1), hhmm(b+2), r(b+4)>>8, r(b+4)&0xFF, r(b+5), r(b+6)))
	}
	var extra strings.Builder
	for i := range cnt {
		if i%10 >= 7 && r(i) != 0 {
			extra.WriteString(fmt.Sprintf(" r%d=%d(0x%04X)", i, r(i), r(i)))
		}
	}
	fmt.Printf("       SLOTS[%d]: %s%s\n", addr, strings.Join(parts, " | "), extra.String())
}

// printS2C renders one downlink frame. tag is a timestamp prefix like
// "t=12:21:04.123" (live) or "[12:21:04.123]" (replay; "~" = estimated).
func printS2C(tag string, fr frame) {
	p := fr.payload
	dir := "cloud->inv"
	if fr.device[0]&0x80 != 0 {
		dir = "inv->cloud"
	}
	head := func(kind string) string {
		return fmt.Sprintf("\n>>> s2c %s %s off=%d dev=%x fn=%02x %s",
			kind, tag, fr.off, fr.device, fr.fn, dir)
	}
	if len(p) >= 2 && p[0] == 0x01 {
		switch p[1] {
		case 0x03, 0x04:
			if len(p) == 6 {
				addr, cnt := u16BE(p, 2), u16BE(p, 4)
				if n := protocol.RegName(addr); n != "" {
					fmt.Printf("%s  fn%02x read @%d (0x%04X) x%d  [%s]\n", head("READ "), p[1], addr, addr, cnt, n)
				} else {
					fmt.Printf("%s  fn%02x read @%d (0x%04X) x%d\n", head("READ "), p[1], addr, addr, cnt)
				}
				return
			}
			if dir == "inv->cloud" && len(p) >= 3 && p[2]%2 == 0 && len(p) == 3+int(p[2]) {
				bc := int(p[2])
				vals := make([]string, 0, bc/2)
				for k := 0; k < bc; k += 2 {
					vals = append(vals, fmt.Sprintf("%d", u16BE(p, 3+k)))
				}
				fmt.Printf("%s  fn%02x response x%d: %s\n", head("RRESP"), p[1], bc/2, strings.Join(vals, " "))
				return
			}
		case 0x06:
			if len(p) == 6 {
				addr, val := u16BE(p, 2), u16BE(p, 4)
				kind := "WRITE"
				if dir == "inv->cloud" {
					kind = "WACK "
				}
				if n := protocol.RegName(addr); n != "" {
					fmt.Printf("%s  fn06 @%d (0x%04X) = %d (0x%04X)  [%s]\n", head(kind), addr, addr, val, val, n)
				} else {
					fmt.Printf("%s  fn06 @%d (0x%04X) = %d (0x%04X)\n", head(kind), addr, addr, val, val)
				}
				return
			}
		case 0x10:
			if len(p) >= 7 {
				addr, cnt := u16BE(p, 2), u16BE(p, 4)
				if dir == "cloud->inv" {
					bc := int(p[6])
					if len(p) == 7+bc && bc == 2*cnt {
						if n := protocol.RegName(addr); n != "" {
							fmt.Printf("%s  fn10 write @%d (0x%04X) x%d:  [%s]\n", head("WRITE"), addr, addr, cnt, n)
						} else {
							fmt.Printf("%s  fn10 write @%d (0x%04X) x%d:\n", head("WRITE"), addr, addr, cnt)
						}
						for k := 0; k < cnt; k += 8 {
							line := make([]string, 0, 8)
							for j := k; j < k+8 && j < cnt; j++ {
								line = append(line, fmt.Sprintf("r%d=%d", j, u16BE(p, 7+2*j)))
							}
							fmt.Printf("       %s\n", strings.Join(line, "  "))
						}
						pushDiff(addr, p, cnt)
						if addr >= 48010 && addr < 49000 && cnt == 80 {
							printSlotTable(addr, p, cnt)
						}
						return
					}
				} else if len(p) == 7 {
					if n := protocol.RegName(addr); n != "" {
						fmt.Printf("%s  fn10 write @%d (0x%04X) x%d accepted  [%s]\n", head("WACK "), addr, addr, cnt, n)
						return
					}
					fmt.Printf("%s  fn10 write @%d (0x%04X) x%d accepted\n", head("WACK "), addr, addr, cnt)
					return
				}
			}
		}
	}
	if len(p) == 57 {
		fmt.Printf("%s  57B serial query, tail=%s\n", head("QUERY"), hexSpaced(p[len(p)-17:]))
		return
	}
	fmt.Printf("%s  len=%d\n%s", head("OTHER"), len(p), hexdump(p))
}

// ---------- tailing ----------

// tailer follows one capture file and yields complete frames.
type tailer struct {
	f       *os.File
	path    string
	offset  int64
	pending []byte
}

func openTailer(p string) *tailer {
	f, err := os.Open(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	size, _ := f.Seek(0, io.SeekEnd)
	return &tailer{f: f, path: p, offset: size}
}

// poll reads new bytes since offset and returns the complete frames in them.
func (t *tailer) poll() []frame {
	sz, err := t.f.Seek(0, io.SeekEnd)
	if err != nil || sz <= t.offset {
		return nil
	}
	n := int(sz - t.offset)
	chunk := make([]byte, n)
	if _, err := t.f.ReadAt(chunk, t.offset); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "read:", err)
		return nil
	}
	t.offset = sz
	t.pending = append(t.pending, chunk...)
	frames, consumed := scan(t.pending, t.offset-int64(len(t.pending)))
	for j := range frames {
		frames[j].detected = time.Now()
	}
	if consumed > 0 {
		t.pending = append([]byte(nil), t.pending[consumed:]...)
	}
	return frames
}

// newestCapture returns the c2s.bin with the newest mtime under the capture
// root; "" if none. The prox creates one directory per cloud connection
// (named by start timestamp) under a per-date parent, so newest-mtime
// tracks both inverter reconnects and the midnight date rollover.
func newestCapture() string {
	best, bestT := "", time.Time{}
	matches, _ := filepath.Glob("/var/lib/prox/conns/*/*/c2s.bin")
	if m2, _ := filepath.Glob("/var/lib/prox/conns/*/c2s.bin"); len(m2) > 0 {
		matches = append(matches, m2...)
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(bestT) {
			best, bestT = m, fi.ModTime()
		}
	}
	return best
}

// ---------- replay (historical) ----------

// s2cWrite is one bytes_s2c meta event: the transmission time of the s2c
// byte range [start, end) (per-write timestamps; the 11-09 and later
// captures carry them, the older ones don't).
type s2cWrite struct {
	ts         time.Time
	start, end int64
}

// connMeta parses the connection's meta.jsonl: the conn_open/conn_close ts
// and the s2c write records; zero/empty when the file is absent.
type connMeta struct {
	open, close time.Time
	writes      []s2cWrite // ordered by start
	drops       []s2cWrite // s2c_drop ranges: captured, never delivered
}

// respLag is the assumed inverter response latency for the correlated
// c2s anchors (observed 27-36 ms live; 50 ms keeps each anchor inside
// the write -> next-write bracket without claiming precision).
const respLag = 50 * time.Millisecond

// assignC2SClock stamps each c2s frame with its best time.
//
// Two anchor kinds are merged in byte order. Correlated anchors pin the
// handshake region to the exact s2c clock: for each s2c register read,
// the inverter's response (device lead | 0x80, the cfg91_* class) is
// stamped at the read's meta write ts + respLag. Period anchors cover
// the steady state: the tele heartbeat when >=2 exist, else the steady
// 30 s keepalive cadence, end-anchored (the kth of N lands at t0 +
// span*(k+1)/N); the cluster keepalive — a handshake carrier at t0, not a
// period point — is dropped from the keepalive set when a correlated
// anchor exists after it, which keeps the first interval from landing one
// cadence late (t0+span/N). Every other frame interpolates linearly in
// bytes between the surrounding anchors; a virtual anchor at (t0, byte 0)
// runs the pre-first-anchor region from t0, and frames after the last
// anchor clamp to it. With no usable anchor pair the whole file falls
// back to byte fractions over t0..t_end (est marks every row).
// clockAnchor is a (byte offset, time) pair the c2s clock interpolates
// between.
type clockAnchor struct {
	b  int64
	at time.Time
}

// buildC2SAnchors assembles the sorted c2s anchor set. Correlated anchors
// pin the handshake region to the exact s2c clock: for each s2c register
// read, the inverter's response (device lead | 0x80, the cfg91_* class) is
// stamped at the read's meta write ts + respLag. Period anchors cover the
// steady state: the tele heartbeat when >=2 exist, else the steady 30 s
// keepalive cadence, end-anchored (the kth of N lands at t0 +
// span*(k+1)/N); the cluster keepalive — a handshake carrier at t0, not a
// period point — is dropped from the keepalive set when a correlated
// anchor exists after it, which keeps the first interval from landing one
// cadence late (t0+span/N).
func buildC2SAnchors(cf []frame, kaIdx, teleIdx []int, sf []frame, writes []s2cWrite, t0 time.Time, span time.Duration) []clockAnchor {
	var anchors []clockAnchor

	// Correlated: s2c read -> c2s response (device lead | 0x80).
	for _, w := range writes {
		var wf *frame
		for i := range sf {
			if sf[i].off == w.start {
				wf = &sf[i]
				break
			}
		}
		if wf == nil {
			continue
		}
		rdev := wf.device
		rdev[0] |= 0x80
		for i := range cf {
			if cf[i].device == rdev && strings.HasPrefix(classify(cf[i]), "cfg91_") {
				anchors = append(anchors, clockAnchor{
					b:  cf[i].off + int64(len(cf[i].payload)) + 13,
					at: w.ts.Add(respLag),
				})
				break // first response frame for this write
			}
		}
	}

	// Period: tele heartbeat, else the steady keepalive cadence.
	period := teleIdx
	periodIsKa := false
	if len(period) < 2 && len(kaIdx) >= 2 {
		period = kaIdx
		periodIsKa = true
	}
	firstCorrelated := int64(0)
	haveCorrelated := false
	for _, a := range anchors {
		if !haveCorrelated || a.b < firstCorrelated {
			firstCorrelated, haveCorrelated = a.b, true
		}
	}
	var periodIdx []int
	for _, idx := range period {
		// The cluster keepalive is the handshake carrier at t0, not a
		// period point: keeping it end-anchors the first interval at
		// t0+span/N (one full cadence late).
		if idx == period[0] && len(period) >= 2 && periodIsKa &&
			haveCorrelated && cf[idx].off+int64(len(cf[idx].payload))+13 < firstCorrelated {
			continue
		}
		periodIdx = append(periodIdx, idx)
	}
	for k, idx := range periodIdx {
		anchors = append(anchors, clockAnchor{
			b:  cf[idx].off + int64(len(cf[idx].payload)) + 13,
			at: t0.Add(time.Duration(float64(span) * float64(k+1) / float64(len(periodIdx)))),
		})
	}
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].b < anchors[j].b })
	return anchors
}

func assignC2SClock(cf []frame, c2sLen int64, kaIdx, teleIdx []int, sf []frame, writes []s2cWrite, t0 time.Time, span time.Duration) (ctime []time.Time, est []bool) {
	anchors := buildC2SAnchors(cf, kaIdx, teleIdx, sf, writes, t0, span)

	ctime = make([]time.Time, len(cf))
	est = make([]bool, len(cf))
	if len(anchors) >= 2 {
		anchors = append([]clockAnchor{{b: 0, at: t0}}, anchors...)
		for i := range cf {
			b := cf[i].off + int64(len(cf[i].payload)) + 13
			switch {
			case b <= anchors[0].b:
				ctime[i] = anchors[0].at
			case b >= anchors[len(anchors)-1].b:
				ctime[i] = anchors[len(anchors)-1].at
			default:
				for k := 1; k < len(anchors); k++ {
					if b <= anchors[k].b {
						ta := anchors[k-1].at
						frac := float64(b-anchors[k-1].b) / float64(anchors[k].b-anchors[k-1].b)
						// scale the delta in ns; never route the fraction
						// through .Seconds() (an int64 ns duration would
						// truncate sub-second offsets to zero).
						ctime[i] = ta.Add(time.Duration(float64(anchors[k].at.Sub(ta)) * frac))
						break
					}
				}
			}
		}
	} else {
		for i := range cf {
			if c2sLen > 0 && span > 0 {
				ctime[i] = t0.Add(time.Duration(float64(span) * float64(cf[i].off+int64(len(cf[i].payload))+13) / float64(c2sLen)))
			} else {
				ctime[i] = t0
			}
			est[i] = true
		}
	}
	return ctime, est
}

func readConnMeta(dir string) connMeta {
	b, err := os.ReadFile(filepath.Join(dir, "meta.jsonl"))
	if err != nil {
		return connMeta{}
	}
	return parseConnMeta(b)
}

// parseConnMeta extracts the open/close ts and the s2c write/drop records
// from raw meta.jsonl bytes (pure, so the table tests feed it synthetic
// files).
func parseConnMeta(b []byte) connMeta {
	var m connMeta
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Ts    int64  `json:"ts"`
			Start int64  `json:"start"`
			End   int64  `json:"end"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Ts == 0 {
			continue
		}
		t := time.UnixMilli(ev.Ts)
		switch ev.Type {
		case "conn_open":
			m.open = t
		case "conn_close":
			m.close = t
		case "bytes_s2c":
			m.writes = append(m.writes, s2cWrite{ts: t, start: ev.Start, end: ev.End})
		case "s2c_drop":
			m.drops = append(m.drops, s2cWrite{ts: t, start: ev.Start, end: ev.End})
		}
	}
	sort.Slice(m.writes, func(i, j int) bool { return m.writes[i].start < m.writes[j].start })
	return m
}

// s2cWriteAt returns the transmission ts of the write chunk covering off;
// the whole-file close summary is not a chunk.
func s2cWriteAt(m connMeta, off, total int64) (time.Time, bool) {
	for _, w := range m.writes {
		if w.start > off {
			break
		}
		if off < w.end && !(w.start == 0 && w.end == total) {
			return w.ts, true
		}
	}
	return time.Time{}, false
}

// s2cDroppedAt reports whether off lies inside an s2c_drop range — bytes
// the capture recorded but the inverter never received.
func s2cDroppedAt(m connMeta, off int64) bool {
	for _, d := range m.drops {
		if off >= d.start && off < d.end {
			return true
		}
	}
	return false
}

// parseRangeTime converts an HH:MM[:SS] (local) or seconds-since-open value
// to a wall clock; a clock value before base wraps to the next day.
func parseRangeTime(s string, base time.Time) (time.Time, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return base.Add(time.Duration(n) * time.Second), true
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			y, mo, d := base.Date()
			t = time.Date(y, mo, d, t.Hour(), t.Minute(), t.Second(), 0, time.Local)
			if t.Before(base) {
				t = t.AddDate(0, 0, 1)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

type ev struct {
	t   time.Time
	dir string
	fr  frame
	m   bool // time exact (meta/response) rather than estimated
	est bool // c2s: byte-fraction time (no tele anchors)
}

func evKey(e ev) string {
	if e.dir == "c2s" {
		return classify(e.fr)
	}
	return "s2c." + nameS2C(e.fr)
}

// typeGaps collects per-type inter-event gaps over the whole session (not
// the -from/-to range) so the summary can show each stream's cadence.
func typeGaps(evs []ev) map[string][]float64 {
	times := map[string][]time.Time{}
	for _, e := range evs {
		times[evKey(e)] = append(times[evKey(e)], e.t)
	}
	gaps := map[string][]float64{}
	for k, ts := range times {
		for i := 1; i < len(ts); i++ {
			gaps[k] = append(gaps[k], ts[i].Sub(ts[i-1]).Seconds())
		}
	}
	return gaps
}

func summary(counts map[string]int, gaps map[string][]float64) {
	// The "summary: " prefix + bare key=value tokens are a contract with
	// internal/frame's TestCorpusParity (it parses this line); keep the
	// label exactly "summary:" and put notes on the line below.
	fmt.Printf("\nsummary: ")
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			fmt.Printf(" ")
		}
		fmt.Printf("%s=%d", k, counts[k])
		if g, ok := gaps[k]; ok && len(g) > 0 {
			sort.Float64s(g)
			fmt.Printf(" med~%.0fs", g[len(g)/2])
		}
	}
	fmt.Println()
	fmt.Println("  (counts are -from/-to range-limited; medians are whole-session)")
}

// replay rebuilds both streams with reconstructed times and prints the
// merged timeline restricted to [from, to].
func replay(c2sPath string) {
	dir := filepath.Dir(c2sPath)
	s2cPath := *s2cPath
	if s2cPath == "" {
		s2cPath = filepath.Join(dir, "s2c.bin")
	}

	c2sData, err := os.ReadFile(c2sPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	cf, _ := scan(c2sData, 0)
	var s2cData []byte
	var sf []frame
	if b, err := os.ReadFile(s2cPath); err == nil {
		s2cData = b
		sf, _ = scan(b, 0)
	}

	fi, err := os.Stat(c2sPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stat:", err)
		os.Exit(1)
	}
	cm := readConnMeta(dir)
	tEnd := fi.ModTime()
	if !cm.close.IsZero() {
		tEnd = cm.close
	}
	t0 := cm.open
	if t0.IsZero() {
		t0 = tEnd.Add(-time.Duration(len(cf)) * time.Second)
		fmt.Println("note: no conn_open ts in meta.jsonl; t0 back-estimated")
	}
	span := tEnd.Sub(t0)

	// c2s clock: tele heartbeat anchors, else keepalive anchors (steady 30 s
	// device cadence), else byte-fraction over t0..t_end (marked ~). Byte
	// fractions are type-distorted on tele-less sessions (mixed 150 B / 135 B
	// / 451 B payloads), so the 30 s anchor beats them whenever >=2 exist.
	// Non-anchor frames interpolate linearly in bytes between the surrounding
	// anchor times, so sub-30 s streams (e.g. the ~23 s wrapper cadence) keep
	// their cadence instead of collapsing to 0 s.
	var teleIdx, kaIdx []int
	for i, fr := range cf {
		switch classify(fr) {
		case "tele":
			teleIdx = append(teleIdx, i)
		case "keepalive":
			kaIdx = append(kaIdx, i)
		}
	}
	ctime, est := assignC2SClock(cf, int64(len(c2sData)), kaIdx, teleIdx, sf, cm.writes, t0, span)

	rFrom, rTo := t0, tEnd
	if s := *from; s != "" {
		t, ok := parseRangeTime(s, t0)
		if !ok {
			fmt.Fprintln(os.Stderr, "bad -from:", s)
			os.Exit(1)
		}
		rFrom = t
	}
	if s := *to; s != "" {
		t, ok := parseRangeTime(s, t0)
		if !ok {
			fmt.Fprintln(os.Stderr, "bad -to:", s)
			os.Exit(1)
		}
		rTo = t
	}

	// time the s2c frames from the inverter's c2s responses
	type respKey struct {
		fn   byte
		addr int
		cnt  int
	}
	wackIdx := map[respKey][]int{}
	rrespFIFO := map[byte][]int{}
	for i, fr := range cf {
		if fr.device[0]&0x80 == 0 {
			continue
		}
		p := fr.payload
		if len(p) < 3 || p[0] != 0x01 {
			continue
		}
		switch p[1] {
		case 0x10:
			if len(p) >= 6 {
				k := respKey{0x10, u16BE(p, 2), u16BE(p, 4)}
				wackIdx[k] = append(wackIdx[k], i)
			}
		case 0x06:
			if len(p) == 6 {
				k := respKey{0x06, u16BE(p, 2), -1}
				wackIdx[k] = append(wackIdx[k], i)
			}
		case 0x03, 0x04:
			if len(p) > 3 {
				rrespFIFO[p[1]] = append(rrespFIFO[p[1]], i)
			}
		}
	}
	stime := make([]time.Time, len(sf))
	smatch := make([]bool, len(sf))
	sz2c := int64(len(s2cData))
	for i, fr := range sf {
		if t, ok := s2cWriteAt(cm, fr.off, sz2c); ok {
			stime[i], smatch[i] = t, true
			continue
		}
		if fn, addr, cnt, ok := s2cSig(fr); ok {
			if fn == 0x03 || fn == 0x04 {
				if q := rrespFIFO[fn]; len(q) > 0 {
					stime[i] = ctime[q[0]]
					rrespFIFO[fn] = q[1:]
					smatch[i] = true
					continue
				}
			} else if q := wackIdx[respKey{fn, addr, cnt}]; len(q) > 0 {
				stime[i] = ctime[q[0]]
				wackIdx[respKey{fn, addr, cnt}] = q[1:]
				smatch[i] = true
				continue
			}
		}
		if sz2c > 0 && span > 0 {
			stime[i] = t0.Add(time.Duration(float64(span) * float64(fr.off) / float64(sz2c)))
		} else {
			stime[i] = t0
		}
	}

	src := "response-corr"
	if len(cm.writes) > 0 {
		src = fmt.Sprintf("meta, %d writes", len(cm.writes))
	}
	csrc := "byte-fraction"
	switch {
	case len(teleIdx) >= 2:
		csrc = fmt.Sprintf("tele, %d", len(teleIdx))
	case len(kaIdx) >= 2:
		csrc = fmt.Sprintf("keepalive, %d", len(kaIdx))
	}
	fmt.Printf("replay %s\n  t0=%s  t_end=%s  span=%.1fs  c2s=%d frames (tele=%d, ts: %s)  s2c=%d frames (ts: %s)\n  range=%s..%s\n",
		c2sPath, t0.Format("01-02 15:04:05.000"), tEnd.Format("01-02 15:04:05.000"), span.Seconds(),
		len(cf), len(teleIdx), csrc, len(sf), src, rFrom.Format("01-02 15:04:05.000"), rTo.Format("01-02 15:04:05.000"))
	evs := make([]ev, 0, len(cf)+len(sf))
	for i, fr := range cf {
		f2 := fr
		f2.detected = ctime[i]
		evs = append(evs, ev{ctime[i], "c2s", f2, true, est[i]})
	}
	for i, fr := range sf {
		f2 := fr
		f2.detected = stime[i]
		evs = append(evs, ev{stime[i], "s2c", f2, smatch[i], false})
	}
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].t.Before(evs[b].t) })

	counts := map[string]int{}
	var prev150 []byte
	for _, e := range evs {
		if e.dir == "c2s" {
			k := classify(e.fr)
			if k == "tele" {
				if e.t.Before(rFrom) || e.t.After(rTo) {
					prev150 = append([]byte(nil), e.fr.payload...)
					continue
				}
				counts[k]++
				print150(e.fr, prev150, nil)
				prev150 = append([]byte(nil), e.fr.payload...)
				continue
			}
			if e.t.Before(rFrom) || e.t.After(rTo) {
				continue
			}
			counts[k]++
			tag := "[" + e.t.Format("15:04:05.000")
			if e.est {
				tag += "~"
			}
			printC2S(tag+"]", k, e.fr, nil)
			continue
		}
		if e.t.Before(rFrom) || e.t.After(rTo) {
			continue
		}
		counts["s2c."+nameS2C(e.fr)]++
		tag := "[" + e.t.Format("15:04:05.000")
		if !e.m {
			tag += "~"
		}
		printS2C(tag+"]", e.fr)
		if s2cDroppedAt(cm, e.fr.off) {
			fmt.Println("    DROPPED: in s2c.bin but never delivered to the inverter (s2c_drop in meta)")
		}
	}
	summary(counts, typeGaps(evs))
}

// ---------- live ----------

func live(c2sPath string, explicit bool) {
	c2s := openTailer(c2sPath)
	s2cPath := *s2cPath
	s2cExplicit := s2cPath != ""
	if s2cPath == "" {
		s2cPath = filepath.Join(filepath.Dir(c2s.path), "s2c.bin")
	}
	var s2c *tailer
	if fi, err := os.Stat(s2cPath); err == nil && !fi.IsDir() {
		s2c = openTailer(s2cPath)
	}
	fmt.Printf("tailing %s (starting at byte %d)\n", c2s.path, c2s.offset)
	if s2c != nil {
		fmt.Printf("tailing %s (starting at byte %d)\n", s2c.path, s2c.offset)
	}
	ha := startHA(*haURL)

	var prev150 []byte
	counts := map[string]int{}
	deadline := time.Now().Add(*dur)
	ticker := time.NewTicker(*pollIv)
	defer ticker.Stop()

	handle := func(dir string, fr frame) {
		if dir == "c2s" {
			k := classify(fr)
			counts[k]++
			if k == "tele" {
				print150(fr, prev150, ha)
				prev150 = append([]byte(nil), fr.payload...)
			} else {
				printC2S("---", k, fr, ha)
			}
			return
		}
		counts["s2c."+nameS2C(fr)]++
		printS2C("t="+fr.detected.Format("15:04:05.000"), fr)
	}

	for time.Now().Before(deadline) {
		<-ticker.C
		if !explicit {
			if np := newestCapture(); np != "" && np != c2s.path {
				fmt.Printf("\n!!! capture file rotated: %s\n", np)
				c2s.f.Close()
				nf, err := os.Open(np)
				if err != nil {
					fmt.Fprintln(os.Stderr, "open:", err)
					return
				}
				c2s = &tailer{f: nf, path: np}
				if s2c != nil && !s2cExplicit {
					s2c.f.Close()
					s2c = nil
					sp := filepath.Join(filepath.Dir(np), "s2c.bin")
					if _, err := os.Open(sp); err == nil {
						s2c = openTailer(sp)
					}
				}
				continue
			}
		}
		for _, fr := range c2s.poll() {
			handle("c2s", fr)
		}
		if s2c != nil {
			for _, fr := range s2c.poll() {
				handle("s2c", fr)
			}
		}
	}
	summary(counts, nil)
}

func main() {
	flag.Parse()
	explicit := false
	flag.Visit(func(fl *flag.Flag) {
		if fl.Name == "file" {
			explicit = true
		}
	})
	path := *filePath
	if !explicit {
		if np := newestCapture(); np != "" {
			path = np
		} else {
			fmt.Fprintln(os.Stderr, "mon150: no capture found under /var/lib/prox/conns — pass -file explicitly")
			os.Exit(2)
		}
	}
	if *from != "" || *to != "" {
		replay(path)
		return
	}
	live(path, explicit)
}
