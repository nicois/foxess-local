package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestStateFromRegisterReads: with no confirmed writes at all (the
// forward=off era — nothing writes the settings), the settings poll's
// register reads fill work mode, meter compensation and the limits.
func TestStateFromRegisterReads(t *testing.T) {
	h := newHarness(t)
	t0 := testBase
	h.seedTele(t0, 1000, -50, 900, 950, 0, 90, 2)
	h.st.RecordRegisters(t0, regWorkModeAddr, []uint16{1})
	block := make([]uint16, 40) // 41007 ×40, the cloud's readback block
	// limits are U32 pairs, high word first (r0=0 r1=W in the captures)
	block[regImportLimAddr+1-41007] = 12500
	block[regExportLimAddr+1-41007] = 5000
	block[regMeterCompAddr-41007] = 0xFFCD // -51
	block[41025-41007] = 777               // not a limit (cloud write test 2026-11-16)
	block[41027-41007] = 560               // import limit current 56.0 A
	block[41028-41007] = 400               // export limit current 40.0 A
	h.st.RecordRegisters(t0, 41007, block)
	h.st.RecordRegisters(t0, regModeEnAddr, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	h.st.Flush()

	st := h.getState(t)
	if st.WorkMode != workModeFeedIn || st.MeterCompensationW != -51 ||
		st.Limits.ImportW != 12500 || st.Limits.ExportW != 5000 || !st.Schedule.Enabled {
		t.Fatalf("from reads: mode %q comp %d limits %+v sched %v",
			st.WorkMode, st.MeterCompensationW, st.Limits, st.Schedule.Enabled)
	}

	if c := st.Limits.ImportCurrentA; c == nil || *c != 56.0 {
		t.Fatalf("import_current_a: %v, want 56.0", c)
	}
	if c := st.Limits.ExportCurrentA; c == nil || *c != 40.0 {
		t.Fatalf("export_current_a: %v, want 40.0", c)
	}

	// an operator write after the read wins until the next read
	h.seedMbWrite(t0.Add(time.Minute), fn10Pdu(regExportLimAddr, []int{0, 3000}))
	if st := h.getState(t); st.Limits.ExportW != 3000 {
		t.Fatalf("newer write: export %d, want 3000", st.Limits.ExportW)
	}
	// …and a later read (the device's own value) wins over the write
	h.st.RecordRegisters(t0.Add(2*time.Minute), regExportLimAddr, []uint16{0, 3000 - 5})
	h.st.Flush()
	if st := h.getState(t); st.Limits.ExportW != 2995 {
		t.Fatalf("newer read: export %d, want 2995", st.Limits.ExportW)
	}
}

func (h *harness) getState(t *testing.T) stateBody {
	t.Helper()
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: %d %s", rr.Code, rr.Body.String())
	}
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestStateCurrentLimitsUnread: before the first settings read the
// current limits are null (unknown), not 0 A.
func TestStateCurrentLimitsUnread(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if !contains(rr.Body.String(), `"import_current_a":null`) || !contains(rr.Body.String(), `"export_current_a":null`) {
		t.Fatalf("unread current limits not null: %s", rr.Body.String())
	}
}
