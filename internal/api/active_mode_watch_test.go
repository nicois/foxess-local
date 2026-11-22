package api

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// TestRunActiveModeBoundary (synctest): when a schedule window starts on
// the inverter's clock, the loop publishes a config event (section
// active-mode) carrying the new running mode — the dashboard's "Running
// now" follows it without a reload. Nothing is published before.
func TestRunActiveModeBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		start := time.Now()
		h.srv.clock = time.Now
		h.st.RecordRegisters(start, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		h.st.RecordRegisters(start, 48010, ownerTable())
		// inverter wall time 17:45:20; the force discharge starts at 17:50
		h.st.RecordRegisters(start, 40000, []uint16{2026, 11, 8, 17, 45, 20})
		sub := h.bus.Subscribe()
		defer sub.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go h.srv.RunActiveMode(ctx)

		time.Sleep(4*time.Minute + 39*time.Second) // 17:49:59
		synctest.Wait()
		if n := len(sub.Events()); n != 0 {
			t.Fatalf("%d events before the window starts", n)
		}
		time.Sleep(3 * time.Second) // 17:50:02
		synctest.Wait()
		select {
		case ev := <-sub.Events():
			ce, ok := ev.Data.(ConfigEvent)
			if ev.Kind != EventConfig || !ok || !slices.Equal(ce.Changed, []string{SectionActiveMode}) {
				t.Fatalf("event %s %+v", ev.Kind, ev.Data)
			}
			if am := ce.State.ActiveMode; am == nil || am.Mode != workModeForceDischarge || am.Group != 4 {
				t.Fatalf("active_mode %+v", am)
			}
		default:
			t.Fatal("no event when the window started")
		}
		time.Sleep(2 * time.Minute) // still inside the window: no repeat
		synctest.Wait()
		if n := len(sub.Events()); n != 0 {
			t.Fatalf("%d repeated events inside the window", n)
		}
		cancel()
		synctest.Wait()
	})
}
