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

// TestRunActiveModeWindowEnd (synctest): when a schedule window ends on
// the inverter's clock, the loop publishes a config event (section
// active-mode) carrying the mode it falls back to — the "Running now"
// line follows the window's end without a reload, as it follows its
// start.
func TestRunActiveModeWindowEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		start := time.Now()
		h.srv.clock = time.Now
		h.st.RecordRegisters(start, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		h.st.RecordRegisters(start, 48010, ownerTable())
		// inverter wall time 19:59:20, inside group 4's force discharge
		// (17:50–20:00); it ends at 20:00
		h.st.RecordRegisters(start, 40000, []uint16{2026, 11, 8, 19, 59, 20})
		sub := h.bus.Subscribe()
		defer sub.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go h.srv.RunActiveMode(ctx)

		time.Sleep(30 * time.Second) // 19:59:50, still inside the window
		synctest.Wait()
		if n := len(sub.Events()); n != 0 {
			t.Fatalf("%d events before the window ends", n)
		}
		time.Sleep(15 * time.Second) // 20:00:05, past the 20:00:01 tick
		synctest.Wait()
		select {
		case ev := <-sub.Events():
			ce, ok := ev.Data.(ConfigEvent)
			if ev.Kind != EventConfig || !ok || !slices.Equal(ce.Changed, []string{SectionActiveMode}) {
				t.Fatalf("event %s %+v", ev.Kind, ev.Data)
			}
			if am := ce.State.ActiveMode; am == nil || am.Mode != workModeSelfUse || am.Group != 6 {
				t.Fatalf("active_mode %+v", am)
			}
		default:
			t.Fatal("no event when the window ended")
		}
		time.Sleep(2 * time.Minute) // after the window: no repeat
		synctest.Wait()
		if n := len(sub.Events()); n != 0 {
			t.Fatalf("%d repeated events after the window", n)
		}
		cancel()
		synctest.Wait()
	})
}
