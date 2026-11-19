package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foxesslocal/internal/config"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/store"
)

// echoInverter records the injected frames and echoes each fn10 write.
type echoInverter struct {
	mu  sync.Mutex
	gw  *gateway.Gateway
	got [][]byte
}

func (e *echoInverter) InjectS2C(_ string, raw []byte) error {
	frs, _ := frame.Split(raw, 0)
	for _, fr := range frs {
		e.mu.Lock()
		e.got = append(e.got, fr.Payload)
		e.mu.Unlock()
		echo := append([]byte{}, fr.Payload[:6]...)
		go func() {
			for _, p := range e.gw.Pending() {
				if p.Status == gateway.StatusInFlight {
					_ = e.gw.Ack(p.ID, frame.Frame{Device: [4]byte{0x91}, Fn: 0x10, Payload: echo})
				}
			}
		}()
	}
	return nil
}

// TestClockSyncWiring (owner, 2026-11-19): the handle's clock write goes
// through the gateway as one fn10 40000 ×6 and returns on the echo; the
// forward windows' wall time is the inverter's clock, else the site
// zone's, never the host's.
func TestClockSyncWiring(t *testing.T) {
	now := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC)
	clk := func() time.Time { return now }
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	set, err := settings.New(st, &config.Config{}, clk)
	if err != nil {
		t.Fatal(err)
	}
	inv := &echoInverter{}
	gw := gateway.New(inv, clk)
	inv.gw = gw
	gw.SetFlow("c1")
	h := &handle{st: st, gw: gw, set: set, clk: clk}

	if _, ok := h.wallNow(); ok {
		t.Fatal("a wall time with no reading and no zone")
	}
	if err := set.Set(settings.KeySiteTimeZone, "Australia/Melbourne", "installer"); err != nil {
		t.Fatal(err)
	}
	if w, ok := h.wallNow(); !ok || w.Hour() != 16 || w.Minute() != 17 {
		t.Fatalf("zone wall time %v %v", w, ok)
	}
	st.RecordRegisters(now, 0x9C40, []uint16{2026, 11, 19, 16, 20, 26}) // the inverter, 3 min fast
	st.Flush()
	if w, ok := h.wallNow(); !ok || w.Minute() != 20 {
		t.Fatalf("inverter wall time %v %v", w, ok)
	}

	if err := h.setClock(context.Background(), [6]int{2026, 11, 19, 16, 17, 26}); err != nil {
		t.Fatal(err)
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	want := []byte{0x01, 0x10, 0x9c, 0x40, 0x00, 0x06, 0x0c, 0x07, 0xea, 0x00, 0x0b, 0x00, 0x13, 0x00, 0x10, 0x00, 0x11, 0x00, 0x1a}
	if len(inv.got) != 1 || string(inv.got[0][:len(want)]) != string(want) {
		t.Fatalf("written %x", inv.got)
	}
}
