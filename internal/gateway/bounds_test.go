package gateway

import (
	"errors"
	"testing"
)

// TestScheduleClearedDisabledSlot: a disabled slot with no field values
// (absent params, or all zero: the API's, the simulator's and the forward
// scheduler's "clear this group") is written the way the cloud writes an
// unused group: work mode 1, max:min SoC 100:10, fd SoC 10, fd power 0
// (the 48010 reads of 2026-11-04 and 2026-11-16, groups 4-8). Against the
// captured table, clearing group 6 is therefore a no-op push.
func TestScheduleClearedDisabledSlot(t *testing.T) {
	gw, _ := newTestGW(t, &vclock{now: fixedNow()})
	cur := currentSet()
	for name, clearSlot := range map[string]func(c *Command){
		"absent": func(c *Command) {
			for _, f := range []SlotField{sStart, sEnd, sWorkMode, sMaxSoc, sMinSoc, sFdSoc, sFdPwr} {
				delete(c.Params, SlotParam(6, f))
			}
		},
		"zeroed": func(c *Command) {
			for _, f := range []SlotField{sStart, sEnd, sWorkMode, sMaxSoc, sMinSoc, sFdSoc, sFdPwr} {
				setSlot(c, 6, f, 0)
			}
		},
	} {
		c := baseSchedule()
		clearSlot(&c)
		frs, err := gw.BuildFrames(c, cur, 0x10)
		if err != nil {
			t.Fatalf("%s: BuildFrames: %v", name, err)
		}
		writes := writeBlocks(t, rawsOf(t, frs[13:]))
		if d := delta(blockVals(cur, 48010, 80), writes[48010]); len(d) != 0 {
			t.Errorf("%s: 48010 Δ = %v, want empty (the cloud's unused-group shape)", name, d)
		}
		for _, base := range mirrorBases {
			if d := delta(blockVals(cur, base, 80), writes[base]); len(d) != 0 {
				t.Errorf("%s: mirror %d Δ = %v, want empty", name, base, d)
			}
		}
	}
}

// TestScheduleEnabledSlotBounds: an enabled slot's SoC fields are 10..100
// and its fd power 0..10500 W (the installer schema's
// k121__segmented_time_mode ranges, dump 2026-11-16).
func TestScheduleEnabledSlotBounds(t *testing.T) {
	gw, _ := newTestGW(t, &vclock{now: fixedNow()})
	cur := currentSet()
	for _, tc := range []struct {
		field SlotField
		v     int
		ok    bool
	}{
		{sMaxSoc, 9, false},
		{sMaxSoc, 10, true},
		{sMinSoc, 9, false},
		{sMinSoc, 10, true},
		{sFdSoc, 9, false},
		{sFdSoc, 10, true},
		{sFdPwr, 10501, false},
		{sFdPwr, 10500, true},
		{sFdPwr, 0, true},
	} {
		c := baseSchedule()
		setSlot(&c, 1, tc.field, tc.v)
		_, err := gw.BuildFrames(c, cur, 0x10)
		if tc.ok && err != nil {
			t.Errorf("slot 1 %s = %d: want ok, got %v", tc.field, tc.v, err)
		}
		if !tc.ok && !errors.Is(err, ErrOutOfRange) {
			t.Errorf("slot 1 %s = %d: want ErrOutOfRange, got %v", tc.field, tc.v, err)
		}
	}
}
