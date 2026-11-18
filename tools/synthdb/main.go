// Command synthdb builds a synthetic foxess-local database at the live
// rates, for load tests and profiling at the scale a device accumulates
// (a 30-day raw retention is ~1.2 million frames). It writes through the
// store's own Record* path, so the schema, indexes and derived rows
// (telemetry, registers, rollup buckets) are the real ones. Payloads are
// synthetic: no serial number, address or reading from a real inverter.
//
//	go run ./tools/synthdb -out /tmp/synth.db -days 30
//
// Rates (from the live database, 2026-11-17): one poll read + response
// every 5 s (the two live-value blocks alternate), a telemetry row every
// 10 s, a keepalive every 30 s, a six-frame snapshot burst and the
// settings reads every 5 minutes, a session (registration, bootstrap,
// audit rows) every 6 hours, and a few operator writes a day.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/store"
)

const (
	pollEvery     = 5 * time.Second
	keepaliveEach = 30 * time.Second
	burstEach     = 5 * time.Minute
	sessionEach   = 6 * time.Hour
	writesPerDay  = 4
)

// synthSN is an obviously synthetic serial number (15 chars, the wire width).
const synthSN = "SYNTH0000000001"

func main() {
	out := flag.String("out", "", "database path to create (must not exist)")
	days := flag.Int("days", 30, "days of data, ending now")
	seed := flag.Uint64("seed", 1, "random seed (the same seed builds the same data)")
	flag.Parse()
	if *out == "" || *days < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*out); err == nil {
		log.Fatalf("synthdb: %s exists; refusing to add to it", *out)
	}
	st, err := store.Open(*out, "synthdb")
	if err != nil {
		log.Fatal(err)
	}
	g := &gen{st: st, rng: rand.New(rand.NewPCG(*seed, *seed^0x5eed)), soc: 50} //nolint:gosec // G404: synthetic data, not security
	end := time.Now().UTC().Truncate(pollEvery)
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)
	t0 := time.Now()
	for t := start; t.Before(end); t = t.Add(sessionEach) {
		g.session(t, minTime(t.Add(sessionEach), end))
	}
	st.Flush()
	n, err := st.RollUp()
	if err != nil {
		log.Fatal(err)
	}
	if err := st.Close(); err != nil {
		log.Fatal(err)
	}
	fi, _ := os.Stat(*out)
	fmt.Printf("synthdb: %s — %d days, %d frames, %d telemetry rows, %d rollup buckets, %.1f MB, built in %v\n",
		*out, *days, g.frames, g.teles, n, float64(fi.Size())/1e6, time.Since(t0).Round(time.Second))
}

type gen struct {
	st     *store.Store
	rng    *rand.Rand
	conn   string
	seq    int
	off    int64
	soc    float64
	frames int
	teles  int
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// session generates one inverter session [from, to).
func (g *gen) session(from, to time.Time) {
	g.conn = fmt.Sprintf("synth-%d", from.Unix())
	g.seq, g.off = 0, 0
	for _, k := range []string{"flow-open", "serve", "tls"} {
		g.st.RecordAudit(from, k, `{"conn":"`+g.conn+`"}`)
	}
	g.frame(from, capture.DirC2S, frame.KindRegister, store.DecisionForwarded, "", g.bytes(23))
	g.frame(from, capture.DirS2C, frame.KindBootstrapResp, store.DecisionDropped, "policy: bootstrap", g.bytes(5))
	block := 0
	writeEvery := 24 * time.Hour / writesPerDay
	for t := from; t.Before(to); t = t.Add(pollEvery) {
		g.poll(t, block)
		if block == 1 {
			g.tele(t)
		}
		block ^= 1
		el := t.Sub(from)
		if el%keepaliveEach == 0 {
			g.frame(t, capture.DirC2S, frame.KindKeepalive, store.DecisionForwarded, "", g.bytes(135))
		}
		if el%burstEach == 0 {
			g.burst(t)
			g.settings(t)
		}
		if el%writeEvery == writeEvery/2 {
			g.write(t)
		}
	}
	g.st.RecordConn(capture.CloseInfo{
		ID: g.conn, Kind: "tls", SNI: "", Client: "192.0.2.10:40000", Dst: "198.51.100.1:14431",
		Opened: from, Closed: to, Cause: "",
	}, 0)
	g.st.RecordAudit(to, "flow-close", `{"conn":"`+g.conn+`"}`)
}

func (g *gen) frame(ts time.Time, dir capture.Dir, class frame.Kind, decision store.Decision, reason string, payload []byte) {
	g.seq++
	fn := byte(0)
	if len(payload) > 1 {
		fn = payload[1]
	}
	g.st.RecordFrame(store.FrameRecord{
		ConnID: g.conn, Seq: g.seq, Dir: dir, Off: g.off, TS: ts, Class: class,
		Dev: [4]byte{0x06}, Fn: fn, Plen: len(payload), Payload: payload,
		Decision: decision, Reason: reason,
	})
	g.off += int64(len(payload))
	g.frames++
}

func (g *gen) bytes(n int) []byte {
	p := make([]byte, n+8)
	for i := 0; i < n; i += 8 {
		binary.LittleEndian.PutUint64(p[i:], g.rng.Uint64())
	}
	return p[:n]
}

// poll is one live-value read (KH realtime block A or B) and its response.
func (g *gen) poll(t time.Time, block int) {
	addr, n := frame.KHRealtimeAAddr, 29
	if block == 1 {
		addr, n = frame.KHRealtimeBAddr, 16
	}
	g.frame(t, capture.DirS2C, frame.KindMbread, store.DecisionInjected, "", readPDU(addr, n))
	g.frame(t.Add(300*time.Millisecond), capture.DirC2S, frame.KindCfg91, store.DecisionForwarded, "", readResp(g.bytes(2*n)))
}

// tele is one decoded live sample: a clear-sky solar day, a base load
// with evening peaks, the battery taking the difference within its SoC.
func (g *gen) tele(t time.Time) {
	h := float64(t.Hour()) + float64(t.Minute())/60
	sol := 0.0
	if h > 6 && h < 19 {
		sol = 9000 * math.Sin(math.Pi*(h-6)/13) * (0.85 + 0.15*g.rng.Float64())
	}
	load := 450 + 250*g.rng.Float64()
	if h > 17 && h < 21 {
		load += 2500
	}
	batt := math.Max(-5000, math.Min(5000, sol-load))
	if (batt > 0 && g.soc >= 100) || (batt < 0 && g.soc <= 10) {
		batt = 0
	}
	g.soc = math.Max(10, math.Min(100, g.soc+batt*10/3600/41930*100))
	grid := load + batt - sol
	g.st.RecordTelemetry(g.conn, t, frame.Tele{
		Sol: int32(sol), Haus: int32(load), Batt: int32(batt), Grid: int32(grid),
		Acport: int32(sol - batt), Soc: byte(g.soc), ChargeState: 2,
	})
	g.teles++
}

// burst is the five-minute snapshot series.
func (g *gen) burst(t time.Time) {
	g.frame(t, capture.DirC2S, frame.KindSnap01, store.DecisionForwarded, "", snap01())
	g.frame(t, capture.DirC2S, frame.KindSnap02, store.DecisionForwarded, "", g.bytes(425))
	g.frame(t, capture.DirC2S, frame.KindSnap04, store.DecisionForwarded, "", g.bytes(202))
	g.frame(t, capture.DirC2S, frame.KindSnap60, store.DecisionForwarded, "", g.bytes(280))
	g.frame(t, capture.DirC2S, frame.KindSnap61, store.DecisionForwarded, "", snap61(uint16(g.soc)))
	g.frame(t, capture.DirC2S, frame.KindSnap65, store.DecisionForwarded, "", g.bytes(165))
	if g.rng.IntN(2) == 0 {
		g.frame(t, capture.DirC2S, frame.KindRSSI, store.DecisionForwarded, "", g.bytes(16))
	}
}

// settings is the settings poll: the reads and the register values.
func (g *gen) settings(t time.Time) {
	for _, b := range []struct{ addr, n int }{{41000, 1}, {41007, 40}, {48000, 10}, {48010, 80}} {
		g.frame(t, capture.DirS2C, frame.KindMbread, store.DecisionInjected, "", readPDU(b.addr, b.n))
		vals := make([]uint16, b.n)
		switch b.addr {
		case 41000:
			vals[0] = 1
		case 41007:
			vals[14], vals[15], vals[16], vals[17] = 0, 12500, 0, 5000 // 41021/41023 limits
		}
		data := make([]byte, 2*b.n)
		for i, v := range vals {
			binary.BigEndian.PutUint16(data[2*i:], v)
		}
		g.frame(t.Add(time.Second), capture.DirC2S, frame.KindCfg91, store.DecisionForwarded, "", readResp(data))
		g.st.RecordRegisters(t.Add(time.Second), b.addr, vals)
	}
}

// write is one operator command (work mode) and its acknowledgement.
func (g *gen) write(t time.Time) {
	v := byte(g.rng.UintN(2))                   //nolint:gosec // G115: 0 or 1
	pdu := []byte{0x01, 0x06, 0xA0, 0x28, 0, v} // 41000 = v
	g.frame(t, capture.DirS2C, frame.KindMbWrite, store.DecisionInjected, "", pdu)
	g.frame(t.Add(500*time.Millisecond), capture.DirC2S, frame.KindMbecho, store.DecisionForwarded, "", pdu)
	d, err := json.Marshal(map[string]string{
		"source": "synth", "source_role": "standard", "action": "work-mode",
		"summary": fmt.Sprintf("work-mode (41000=%d)", v), "confirmed": "acked",
	})
	if err != nil {
		panic(err) // a map of strings always marshals
	}
	g.st.RecordAudit(t, "command", string(d))
}

func readPDU(addr, n int) []byte {
	p := []byte{0x01, 0x03, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(p[2:], uint16(addr)) //nolint:gosec // G115: a register address.
	binary.BigEndian.PutUint16(p[4:], uint16(n))    //nolint:gosec // G115: a register count.
	return p
}

func readResp(data []byte) []byte {
	return append([]byte{0x01, 0x03, byte(len(data))}, data...) //nolint:gosec // G115: at most 160 data bytes
}

// snap01 is the device-info snapshot (synthetic SN, model KH10, fw 1.73).
func snap01() []byte {
	p := make([]byte, 141)
	copy(p[0:15], synthSN)
	copy(p[33:37], "1.73")
	copy(p[39:43], "1.03")
	copy(p[45:49], "1.71")
	copy(p[51:55], "0.00")
	copy(p[93:109], "KH10            ")
	return p
}

// snap61 is the battery snapshot (SoC and a stored-charge counter at the
// profile §4 TLV layout).
func snap61(soc uint16) []byte {
	p := make([]byte, 626)
	copy(p, synthSN)
	put := func(base, idx, tag, off int, v uint16) {
		e := base + 12*idx
		binary.BigEndian.PutUint16(p[e:e+2], uint16(tag)) //nolint:gosec // G115: a protocol tag.
		binary.LittleEndian.PutUint16(p[e+2+off:e+4+off], v)
	}
	put(40, 16, 0x1905, 6, soc)
	put(40, 18, 0x1907, 0, soc*10)
	put(412, 0, 0x0c05, 6, 52000)
	put(412, 1, 0x0c06, 6, 53000)
	return p
}
