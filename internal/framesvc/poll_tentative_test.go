package framesvc

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/fmdata"
	"foxesslocal/internal/frame"
)

// regDevice is a fake inverter backed by a register map: reads of
// absent addresses fail like a Modbus exception.
type regDevice struct {
	mu       sync.Mutex
	regs     map[int]uint16
	reads    []int
	recorded []frame.Tele
	regBlks  []int
}

func (d *regDevice) poller() *poller {
	return &poller{
		target: func(time.Time) (string, bool) { return "c1", true },
		read: func(_ context.Context, _ int, addr, count int) ([]uint16, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.reads = append(d.reads, addr)
			out := make([]uint16, count)
			for i := range count {
				v, ok := d.regs[addr+i]
				if !ok {
					return nil, errors.New("exception 2")
				}
				out[i] = v
			}
			return out, nil
		},
		record: func(_ string, _ time.Time, t frame.Tele) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.recorded = append(d.recorded, t)
		},
		recordRegs: func(_ time.Time, addr int, _ []uint16) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.regBlks = append(d.regBlks, addr)
		},
		observe: func(Event) {},
		clock:   time.Now,
	}
}

// synthH1 is an H1-G2 at a consistent operating point, written through
// the H1_G2_PRE144 map; it answers its planned blocks (gaps included)
// and the work-mode register, nothing else — no KH live blocks, no
// 30016 version block.
func synthH1(t *testing.T) map[int]uint16 {
	t.Helper()
	m, _ := fmdata.Load().Map("H1_G2_PRE144-HOLDING")
	r := map[int]uint16{41000: 0} // work mode: self use
	putModel(r, "H1-5.0-E-G2")
	for key, v := range map[string]float64{
		"pv1_voltage": 300, "pv1_current": 5, "pv1_power": 1.5,
		"pv2_voltage": 250, "pv2_current": 6, "pv2_power": 1.5,
		"rvolt": 240.5, "rfreq": 50.01, "battery_soc": 64,
		"load_power": 1.5, "rpower": 1.9, "grid_ct": 0.4, "invbatpower": -1.0,
	} {
		putValue(t, r, m, key, v)
	}
	for _, c := range compat.Candidates("H1-5.0-E-G2") {
		for _, b := range c.Plan() {
			fillBlock(r, b)
		}
	}
	return r
}

// putModel writes a model string into 30000 ×15 (two chars per register).
func putModel(r map[int]uint16, model string) {
	padded := model + strings.Repeat(" ", 30)
	for i := range 15 {
		r[30000+i] = uint16(padded[2*i])<<8 | uint16(padded[2*i+1])
	}
}

// putValue encodes one physical value through a map entity.
func putValue(t *testing.T, r map[int]uint16, m fmdata.Map, key string, v float64) {
	t.Helper()
	e, ok := m.Entity(key)
	if !ok {
		t.Fatalf("no %s", key)
	}
	if e.Scale != nil {
		v /= *e.Scale
	}
	u := uint32(int64(math.Round(v))) //nolint:gosec // G115: test encoding
	r[e.Addresses[0]] = uint16(u)     //nolint:gosec // G115: low word
	if len(e.Addresses) == 2 {
		r[e.Addresses[1]] = uint16(u >> 16)
	}
}

// fillBlock makes every register of a planned block answer (zero if unset).
func fillBlock(r map[int]uint16, b fmdata.Block) {
	for a := b.Addr; a < b.Addr+b.Count; a++ {
		if _, ok := r[a]; !ok {
			r[a] = 0
		}
	}
}

// countReads counts reads of one address; khSettings reports whether a
// KH settings block was read.
func countReads(reads []int, addr int) (n int, khSettings bool) {
	for _, a := range reads {
		if a == 41007 || a == 48010 {
			khSettings = true
		}
		if a == addr {
			n++
		}
	}
	return n, khSettings
}

// TestPollerTentativeLayout: an H1-G2 (no 30016 block, no KH layout) is
// classified by its foxess_modbus map and polled through that map's read
// plan; each poll is an ordinary telemetry row (PV 3 kW, SoC 64 %), and
// the KH settings blocks are never read.
func TestPollerTentativeLayout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &regDevice{regs: synthH1(t)}
		p := d.poller()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { p.run(ctx); close(done) }()
		time.Sleep(3*pollEvery + pollEvery/2)
		synctest.Wait()
		cancel()
		<-done
		d.mu.Lock()
		defer d.mu.Unlock()
		if p.layout != "H1_G2_PRE144-HOLDING" {
			t.Fatalf("layout %q", p.layout)
		}
		if len(d.recorded) != 3 || d.recorded[0].Sol != 3000 || d.recorded[0].Soc != 64 || d.recorded[0].Batt != 1000 {
			t.Fatalf("telemetry %+v", d.recorded)
		}
		// the map's work-mode register once per settings round; never the
		// KH settings blocks
		if n, kh := countReads(d.reads, 41000); n != 1 || kh {
			t.Fatalf("work mode read %d times (want 1); KH settings read: %v", n, kh)
		}
	})
}
