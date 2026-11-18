package api

import (
	"encoding/json"
	"testing"
)

// The 48010 table as the cloud left it on 2026-11-18 (00:44 UTC push):
// group 1 force charge 11:02–13:58 (target 81 %, 10500 W, after cut-off
// self-use), group 2 force discharge 17:50–20:00 (50 %, 10076 W, after
// cut-off self-use), group 3 the default self-use 00:00–23:59 (100:11).
var cloudSchedule = func() []uint16 {
	t := make([]uint16, 80)
	copy(t[0:10], []uint16{1, 0x0B02, 0x0D3A, 6, 0x640A, 81, 10500, 0, 3, 2})
	copy(t[10:20], []uint16{1, 0x1132, 0x1400, 7, 0x640A, 50, 10076, 0, 3, 2})
	copy(t[20:30], []uint16{1, 0x0000, 0x173B, 1, 0x640B, 10, 0, 0, 1, 2})
	for k := 3; k < 8; k++ {
		t[10*k+9] = 2
	}
	return t
}()

// groupParams is one 57000-family block: import r1:r2, export r3:r4
// (U32, high word first), r6 the from-PV power (0 = off).
func groupParams(imp, exp uint32, pv uint16) []uint16 {
	b := make([]uint16, 50)
	b[0] = 2
	b[1], b[2] = uint16(imp>>16), uint16(imp) //nolint:gosec // G115: test values fit
	b[3], b[4] = uint16(exp>>16), uint16(exp) //nolint:gosec // G115: test values fit
	b[6] = pv
	return b
}

// TestScheduleView (2026-11-18 decode, owner's mode-scheduler session):
// /state's schedule names the default group (the enabled 00:00–23:59
// window: the cloud's "remaining time" mode), each force window's
// action after its cut-off (+8: 1 standby, 3 self-use) and, from the
// group's parameter block (57000 + 50·(k−1)), its import / export limit
// and whether it uses PV.
func TestScheduleView(t *testing.T) {
	h := newHarness(t)
	h.st.RecordRegisters(testBase, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	h.st.RecordRegisters(testBase, 48010, cloudSchedule)
	h.st.RecordRegisters(testBase, 57000, groupParams(35000, 100000, 20000))
	h.st.RecordRegisters(testBase, 57050, groupParams(100000, 10000, 0))
	rec := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	var st struct {
		Schedule struct {
			Groups []viewGroup `json:"groups"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	g := st.Schedule.Groups
	if len(g) != 8 {
		t.Fatalf("%d groups", len(g))
	}
	checkDefaults(t, g)
	checkAfterCutoff(t, g)
	checkGroupParams(t, g)
}

// viewGroup is the read-only part of a /state schedule group.
type viewGroup struct {
	Index        int     `json:"index"`
	Default      bool    `json:"default"`
	AfterCutoff  *string `json:"after_cutoff"`
	ImportLimitW *int    `json:"import_limit_w"`
	ExportLimitW *int    `json:"export_limit_w"`
	PV           *bool   `json:"pv"`
}

func checkDefaults(t *testing.T, g []viewGroup) {
	t.Helper()
	for i, want := range []bool{false, false, true, false} {
		if g[i].Default != want {
			t.Errorf("group %d default %v, want %v", i+1, g[i].Default, want)
		}
	}
}

func checkAfterCutoff(t *testing.T, g []viewGroup) {
	t.Helper()
	for i, want := range []string{workModeSelfUse, workModeSelfUse, ""} {
		got := ""
		if g[i].AfterCutoff != nil {
			got = *g[i].AfterCutoff
		}
		if got != want {
			t.Errorf("group %d after cut-off %q, want %q", i+1, got, want)
		}
	}
}

func checkGroupParams(t *testing.T, g []viewGroup) {
	t.Helper()
	if g[0].ImportLimitW == nil || *g[0].ImportLimitW != 35000 || *g[0].ExportLimitW != 100000 || g[0].PV == nil || !*g[0].PV {
		t.Errorf("group 1 params: %+v", g[0])
	}
	if g[1].ExportLimitW == nil || *g[1].ExportLimitW != 10000 || g[1].PV == nil || *g[1].PV {
		t.Errorf("group 2 params: %+v", g[1])
	}
	if g[2].ImportLimitW != nil || g[3].PV != nil {
		t.Errorf("no block read: %+v %+v", g[2], g[3])
	}
}
