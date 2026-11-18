package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
)

// checkMb parses one injected frame as a Modbus PDU.
func checkMb(t *testing.T, fr frame.Frame) frame.Modbus {
	t.Helper()
	mb, err := frame.ParseModbus(fr.Payload)
	if err != nil {
		t.Fatalf("injected frame is not a Modbus PDU: %v (payload %x)", err, fr.Payload)
	}
	return mb
}

// TestPutWorkMode: valid → 200 with the read-back value; the fake
// inverter receives the fn06 @41000 frame.
func TestPutWorkMode(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /work-mode: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		WorkMode string `json:"work_mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.WorkMode != workModeFeedIn {
		t.Fatalf("read-back: got %q, want feed-in", body.WorkMode)
	}
	frs := h.inv.injected()
	if len(frs) != 1 {
		t.Fatalf("injected %d frames, want 1", len(frs))
	}
	mb := checkMb(t, frs[0])
	if mb.Fn != 0x06 || mb.Addr != regWorkModeAddr || mb.Value != 1 {
		t.Fatalf("frame: got fn=%#04x addr=%d val=%d, want fn06 @41000 val 1", mb.Fn, mb.Addr, mb.Value)
	}
}

// TestPutWorkModeInvalid: an unknown mode is 400 invalid_value with the
// field named.
func TestPutWorkModeInvalid(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: "boost"})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != jsonFieldWorkMode {
		t.Fatalf("errors: got %+v, want field work_mode", p.Errors)
	}
	if n := len(h.inv.injected()); n != 0 {
		t.Fatalf("invalid command injected %d frames, want 0", n)
	}
}

// scheduleWith builds the spec's 8-group table from the two given
// groups (1 and 2), groups 3..8 zeroed (self-use).
func scheduleWith(g1, g2 scheduleGroup) schedule {
	z := func(i int) scheduleGroup {
		return scheduleGroup{Index: i, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse}
	}
	return schedule{Enabled: true, Groups: []scheduleGroup{g1, g2, z(3), z(4), z(5), z(6), z(7), z(8)}}
}

// TestPutSchedule: valid 2-group table → 200 with the normalized table;
// the fake inverter receives the 13-read + 13-write read-modify-write
// push with the command's slots.
func TestPutSchedule(t *testing.T) {
	h := newHarness(t)
	body := scheduleWith(
		scheduleGroup{Index: 1, Enabled: true, Start: testTime1101, End: testTime1359, WorkMode: workModeForceCharge, MaxSocPct: 80, MinSocOnGridPct: 20, FdSocPct: 90, FdPwrW: 9500},
		scheduleGroup{Index: 2, Enabled: true, Start: "18:00", End: "20:00", WorkMode: workModeForceDischarge, MaxSocPct: 50, MinSocOnGridPct: 10, FdSocPct: 30, FdPwrW: 8000})
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /schedule: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var got schedule
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !got.Enabled || len(got.Groups) != 8 {
		t.Fatalf("read-back: enabled=%v groups=%d", got.Enabled, len(got.Groups))
	}
	if got.Groups[0] != body.Groups[0] || got.Groups[1] != body.Groups[1] {
		t.Fatalf("read-back groups: got %+v / %+v", got.Groups[0], got.Groups[1])
	}
	// an unused group reads back as written (what /state then shows)
	wantUnused := scheduleGroup{Index: 3, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 10}
	if got.Groups[2] != wantUnused {
		t.Fatalf("read-back unused group: got %+v, want %+v", got.Groups[2], wantUnused)
	}

	frs := h.inv.injected()
	if len(frs) != 26 {
		t.Fatalf("injected %d frames, want 26 (13 reads + 13 writes)", len(frs))
	}
	assertScheduleReads(t, frs[:13])
	assertScheduleWrites(t, frs[13:])
}

// assertScheduleReads checks the read phase: 48000, 48010, then the
// eleven mirror blocks (the push's block order).
func assertScheduleReads(t *testing.T, frs []frame.Frame) {
	t.Helper()
	wantReads := []struct {
		addr, cnt int
	}{
		{regModeEnAddr, 10}, {regSlotTableAddr, 80},
	}
	for base := 48090; base <= 48890; base += 80 {
		wantReads = append(wantReads, struct {
			addr, cnt int
		}{base, 80})
	}
	for i, want := range wantReads {
		mb := checkMb(t, frs[i])
		if mb.Fn != 0x03 || mb.Addr != want.addr || mb.Count != want.cnt {
			t.Fatalf("read %d: got fn=%#04x addr=%d x%d, want fn03 %d x%d",
				i, mb.Fn, mb.Addr, mb.Count, want.addr, want.cnt)
		}
	}
}

// assertScheduleWrites checks the write phase: 48000 r0 = enabled, the
// 48010 table with the command's slots, and the eleven mirror blocks
// replicating the table's +4/+5/+6 fields.
func assertScheduleWrites(t *testing.T, frs []frame.Frame) {
	t.Helper()
	// Same block order as the read phase.
	w0 := checkMb(t, frs[0])
	if w0.Fn != 0x10 || w0.Addr != regModeEnAddr || w0.Count != 10 || regAt(w0.Data, 0) != 1 {
		t.Fatalf("write 0: got addr=%d x%d r0=%d, want 48000 x10 r0=1", w0.Addr, w0.Count, regAt(w0.Data, 0))
	}
	w1 := checkMb(t, frs[1])
	if w1.Fn != 0x10 || w1.Addr != regSlotTableAddr || w1.Count != 80 {
		t.Fatalf("write 1: got addr=%d x%d, want 48010 x80", w1.Addr, w1.Count)
	}
	slot := func(k int) [7]int {
		b := 10 * k
		return [7]int{
			regAt(w1.Data, b), regAt(w1.Data, b+1), regAt(w1.Data, b+2),
			regAt(w1.Data, b+3), regAt(w1.Data, b+4), regAt(w1.Data, b+5),
			regAt(w1.Data, b+6),
		}
	}
	want1 := [7]int{1, 11<<8 | 1, 13<<8 | 59, 6, 80<<8 | 20, 90, 9500}
	want2 := [7]int{1, 18 << 8, 20 << 8, 7, 50<<8 | 10, 30, 8000}
	if got1, got2 := slot(0), slot(1); got1 != want1 || got2 != want2 {
		t.Fatalf("48010 slots: got %v / %v, want %v / %v", got1, got2, want1, want2)
	}
	// unused groups go out in the cloud's unused-group shape
	unused := [7]int{0, 0, 0, 1, 100<<8 | 10, 10, 0}
	for k := 2; k < 8; k++ {
		if g := slot(k); g != unused {
			t.Fatalf("48010 slot %d (unused): got %v, want %v", k, g, unused)
		}
	}
	// The mirror blocks replicate the table's +4/+5/+6 fields.
	for i := 2; i < len(frs); i++ {
		assertMirrorWrite(t, i-2, checkMb(t, frs[i]))
	}
}

// assertMirrorWrite checks one of the eleven mirror blocks: an fn10 x80
// write replicating the table's +4/+5/+6 fields.
func assertMirrorWrite(t *testing.T, i int, mb frame.Modbus) {
	t.Helper()
	if mb.Fn != 0x10 || mb.Count != 80 {
		t.Fatalf("mirror write %d: got fn=%#04x x%d, want fn10 x80", i, mb.Fn, mb.Count)
	}
	// Group 1 is slot 0: the mirrored +4/+5/+6 sit at registers 4/5/6.
	if regAt(mb.Data, 4) != 80<<8|20 || regAt(mb.Data, 5) != 90 || regAt(mb.Data, 6) != 9500 {
		t.Fatalf("mirror %d group 1: got %d/%d/%d, want the table's +4/+5/+6",
			i, regAt(mb.Data, 4), regAt(mb.Data, 5), regAt(mb.Data, 6))
	}
}

// regAt decodes register r of an fn10 data blob.
func regAt(data []byte, r int) int {
	return int(data[2*r])<<8 | int(data[2*r+1])
}

// TestPutScheduleDisabledMaster: master disabled (enabled:false) is a
// legal push — 48000 r0=0 must be written, not a 400 missing-param
// (regression: the enabled param was only set when true).
func TestPutScheduleDisabledMaster(t *testing.T) {
	h := newHarness(t)
	body := scheduleWith(
		scheduleGroup{Index: 1, Enabled: true, Start: testTime1101, End: testTime1359, WorkMode: workModeForceCharge, MaxSocPct: 80, MinSocOnGridPct: 20, FdSocPct: 90, FdPwrW: 9500},
		scheduleGroup{Index: 2, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse})
	body.Enabled = false
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /schedule (master disabled): got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var got schedule
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got.Enabled {
		t.Fatalf("read-back enabled=%v, want false", got.Enabled)
	}
	frs := h.inv.injected()
	if len(frs) != 26 {
		t.Fatalf("injected %d frames, want 26 (13 reads + 13 writes)", len(frs))
	}
	w0 := checkMb(t, frs[13])
	if w0.Fn != 0x10 || w0.Addr != regModeEnAddr || regAt(w0.Data, 0) != 0 {
		t.Fatalf("write 0: got fn=%#04x addr=%d r0=%d, want fn10 48000 r0=0", w0.Fn, w0.Addr, regAt(w0.Data, 0))
	}
}

// TestPutScheduleOverlap: partially overlapping enabled windows → 400
// schedule_overlap.
func TestPutScheduleOverlap(t *testing.T) {
	h := newHarness(t)
	body := scheduleWith(
		scheduleGroup{Index: 1, Enabled: true, Start: testTime1101, End: testTime1359, WorkMode: workModeForceCharge, MaxSocPct: 80, MinSocOnGridPct: 20, FdSocPct: 90, FdPwrW: 9500},
		scheduleGroup{Index: 2, Enabled: true, Start: "13:00", End: "15:00", WorkMode: workModeForceDischarge, MaxSocPct: 50, MinSocOnGridPct: 10, FdSocPct: 30, FdPwrW: 8000})
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, body)
	checkProblem(t, rr, http.StatusBadRequest, "schedule_overlap")
	if n := len(h.inv.injected()); n != 0 {
		t.Fatalf("overlapping schedule injected %d frames, want 0", n)
	}
}

// TestPutScheduleBadWindow: end <= start on an enabled group → 400
// invalid_value.
func TestPutScheduleBadWindow(t *testing.T) {
	h := newHarness(t)
	body := scheduleWith(
		scheduleGroup{Index: 1, Enabled: true, Start: "12:00", End: "11:00", WorkMode: workModeForceCharge, MaxSocPct: 80, MinSocOnGridPct: 20, FdSocPct: 90, FdPwrW: 9500},
		scheduleGroup{Index: 2, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse})
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, body)
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 {
		t.Fatalf("bad window: want errors[], got none")
	}
}

// TestPutScheduleBounds: an enabled group's SoC fields are 10..100 and
// its fd power 0..10500 W (the installer schema's segmented-time ranges)
// → 400 invalid_value naming the field, nothing sent. A disabled group's
// values are not checked (it is written in the unused-group shape).
func TestPutScheduleBounds(t *testing.T) {
	ok := scheduleGroup{Index: 1, Enabled: true, Start: testTime1101, End: testTime1359, WorkMode: workModeForceCharge, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 10, FdPwrW: 10500}
	off := scheduleGroup{Index: 2, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse}
	for _, tc := range []struct {
		field string
		set   func(g *scheduleGroup)
	}{
		{"groups[0].max_soc_pct", func(g *scheduleGroup) { g.MaxSocPct = 9 }},
		{"groups[0].min_soc_on_grid_pct", func(g *scheduleGroup) { g.MinSocOnGridPct = 9 }},
		{"groups[0].fd_soc_pct", func(g *scheduleGroup) { g.FdSocPct = 101 }},
		{"groups[0].fd_pwr_w", func(g *scheduleGroup) { g.FdPwrW = 10501 }},
	} {
		h := newHarness(t)
		g := ok
		tc.set(&g)
		rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, scheduleWith(g, off))
		p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
		if len(p.Errors) == 0 || p.Errors[0].Field != tc.field {
			t.Fatalf("%s: got errors %+v", tc.field, p.Errors)
		}
		if n := len(h.inv.injected()); n != 0 {
			t.Fatalf("%s: injected %d frames, want 0", tc.field, n)
		}
	}
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, scheduleWith(ok, off))
	if rr.Code != http.StatusOK {
		t.Fatalf("bounds inclusive: got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestPutScheduleGroupCount: not exactly 8 groups → 400 invalid_value.
func TestPutScheduleGroupCount(t *testing.T) {
	h := newHarness(t)
	body := schedule{Enabled: false, Groups: []scheduleGroup{
		{Index: 1, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse},
		{Index: 2, Enabled: false, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse},
	}}
	rr := h.do(t, httpPut, "/api/v1/schedule", RoleStandard, body)
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != "groups" {
		t.Fatalf("group count: got errors %+v, want field groups", p.Errors)
	}
}

// TestPutMeterCompensation: valid → 200 read-back; the frame carries the
// two's-complement value (−50 → 0xFFCE).
func TestPutMeterCompensation(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/meter-compensation", RoleExpert, map[string]int{"w": -50})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /meter-compensation: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		W int `json:"w"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.W != -50 {
		t.Fatalf("read-back: got %d, want -50", body.W)
	}
	frs := h.inv.injected()
	if len(frs) != 1 {
		t.Fatalf("injected %d frames, want 1", len(frs))
	}
	mb := checkMb(t, frs[0])
	if mb.Fn != 0x06 || mb.Addr != regMeterCompAddr || mb.Value != 0xFFCE {
		t.Fatalf("frame: got fn=%#04x addr=%d val=%#04x, want fn06 @41029 0xFFCE", mb.Fn, mb.Addr, mb.Value)
	}
}

// TestPutMeterCompensationOutOfRange: outside −500..500 → 400.
func TestPutMeterCompensationOutOfRange(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/meter-compensation", RoleExpert, map[string]int{"w": 501})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != "w" {
		t.Fatalf("out of range: got errors %+v, want field w", p.Errors)
	}
}

// assertLimitFrame checks one fn10 ×2 limits frame: val in both
// registers at addr (the device doubles the register pair).
func assertLimitFrame(t *testing.T, mb frame.Modbus, addr, val int) {
	t.Helper()
	if mb.Fn != 0x10 || mb.Addr != addr || mb.Count != 2 ||
		regAt(mb.Data, 0) != 0 || regAt(mb.Data, 1) != val {
		t.Fatalf("frame: got addr=%d x%d data=%x, want fn10 @%d x2 U32 hi 0 lo %d", mb.Addr, mb.Count, mb.Data, addr, val)
	}
}

// TestPutLimits: valid → 200 read-back; two fn10 ×2 frames (import
// first).
func TestPutLimits(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/limits", RoleInstaller, map[string]int{jsonFieldImportW: 4973, jsonFieldExportW: 2500})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /limits: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body limitsBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ImportW != 4973 || body.ExportW != 2500 {
		t.Fatalf("read-back: got %+v, want {4973 2500}", body)
	}
	frs := h.inv.injected()
	if len(frs) != 2 {
		t.Fatalf("injected %d frames, want 2", len(frs))
	}
	assertLimitFrame(t, checkMb(t, frs[0]), regImportLimAddr, 4973)
	assertLimitFrame(t, checkMb(t, frs[1]), regExportLimAddr, 2500)
}

// TestPutLimitsPartial: one field only → 200; the absent field's
// read-back is 0 (no device re-read in task 7 — see the report).
func TestPutLimitsPartial(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/limits", RoleInstaller, map[string]int{jsonFieldExportW: 100})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /limits partial: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body limitsBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ImportW != 0 || body.ExportW != 100 {
		t.Fatalf("read-back: got %+v, want {0 100}", body)
	}
	if n := len(h.inv.injected()); n != 1 {
		t.Fatalf("injected %d frames, want 1 (export only)", n)
	}
}

// TestPutLimitsMissing: no fields → 400 invalid_value.
func TestPutLimitsMissing(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/limits", RoleInstaller, map[string]int{})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 {
		t.Fatalf("missing fields: want errors[], got none")
	}
}

// TestPutLimitsOutOfRange: above 30000 → 400 invalid_value.
func TestPutLimitsOutOfRange(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, "/api/v1/limits", RoleInstaller, map[string]int{jsonFieldImportW: 30001})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != jsonFieldImportW {
		t.Fatalf("out of range: got errors %+v, want field import_w", p.Errors)
	}
}

// TestPutCommandInFlight: a second command while one is in flight → 409
// command_in_flight.
func TestPutCommandInFlight(t *testing.T) {
	h := newHarness(t)
	h.inv.setAutoAck(false)
	if _, err := h.gw.Submit(context.Background(), gateway.Command{
		Kind: gateway.KindWorkMode, Params: map[gateway.Param]int{gateway.ParamMode: 1},
	}); err != nil {
		t.Fatalf("seed Submit: %v", err)
	}
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeSelfUse})
	checkProblem(t, rr, http.StatusConflict, "command_in_flight")
}

// TestPutCommandFailed: a device read-back that contradicts the write
// (fn06 echo with the wrong value) fails the command — the response is
// 502 command_failed with the gateway's mismatch detail and
// applied=no, not a 504 claiming a timeout.
func TestPutCommandFailed(t *testing.T) {
	h := newHarness(t)
	h.inv.corruptNextFn06()
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeSelfUse})
	p := checkProblem(t, rr, http.StatusBadGateway, "command_failed")
	if p.Detail == "" || p.Detail == "no device read-back within the timeout" {
		t.Fatalf("detail: %q (want the gateway's mismatch text)", p.Detail)
	}
	if p.Applied != confirmedNo {
		t.Fatalf("applied: %q, want %q", p.Applied, confirmedNo)
	}
}

// TestPutInverterOffline: no active flow → 503 inverter_offline (the
// synchronous write cannot be confirmed without a session).
func TestPutInverterOffline(t *testing.T) {
	h := newHarness(t)
	h.gw.SetFlow("")
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})
	checkProblem(t, rr, http.StatusServiceUnavailable, "inverter_offline")
}

// TestSubmitResponseWriteBudget: a write refused by the gateway's
// per-setting budget is a 429 the UI can explain.
func TestSubmitResponseWriteBudget(t *testing.T) {
	resp := submitResponse(fmt.Errorf("%w: 10 writes in the last 1h0m0s", gateway.ErrWriteBudget))
	p, ok := resp.body.(problem)
	if resp.status != http.StatusTooManyRequests || !ok || p.Code != "write_budget" {
		t.Fatalf("got %d %+v, want 429 write_budget", resp.status, resp.body)
	}
}

// TestAuditUnchanged: an acked command the inverter already held is
// audited as unchanged (nothing was written).
func TestAuditUnchanged(t *testing.T) {
	if got := auditSummary("work-mode feed-in (41000=1)", gateway.Result{Status: gateway.StatusAcked, Unchanged: true}); got != "work-mode feed-in (41000=1) — unchanged, not written" {
		t.Fatalf("summary %q", got)
	}
	if got := auditSummary("x", gateway.Result{Status: gateway.StatusAcked}); got != "x" {
		t.Fatalf("summary %q", got)
	}
}

// TestWriteBudgetAlerts (F2): a write refused by the hourly budget is
// also a system alert (notified), naming the setting and the caller —
// an automation that flips a setting too often is otherwise only a 429
// in its own log.
func TestWriteBudgetAlerts(t *testing.T) {
	h := newHarness(t)
	gateway.WithWriteBudget(2, time.Hour)(h.gw) // as production wires it, smaller
	sub := h.bus.Subscribe()
	defer sub.Close()
	modes := []string{workModeFeedIn, workModeSelfUse}
	var last *httptest.ResponseRecorder
	for i := range 12 {
		last = h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: modes[i%2]})
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	checkProblem(t, last, http.StatusTooManyRequests, "write_budget")
	for {
		select {
		case ev := <-sub.Events():
			se, ok := ev.Data.(SystemEvent)
			if !ok || se.Type != SystemAlert {
				continue
			}
			if !strings.Contains(se.Detail, "work-mode") || !strings.Contains(se.Detail, "tok-standard") {
				t.Fatalf("alert %q", se.Detail)
			}
			return
		default:
			t.Fatal("no alert for the refused write")
		}
	}
}

// TestWriteBudgetVisible (foxess-control local backend, 2026-11-19): the
// budget is in /state per setting (the identity.writes names), and a
// refusal says which setting, that none remain, when the next write is
// allowed, and Retry-After.
func TestWriteBudgetVisible(t *testing.T) {
	h := newHarness(t)
	gateway.WithWriteBudget(2, time.Hour)(h.gw)
	var st struct {
		WriteBudget map[string]struct {
			Limit      int        `json:"limit"`
			Remaining  int        `json:"remaining"`
			WindowS    int        `json:"window_s"`
			NextFreeAt *time.Time `json:"next_free_at"`
		} `json:"write_budget"`
	}
	decode(t, h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil), &st)
	if b := st.WriteBudget["work_mode"]; b.Limit != 2 || b.Remaining != 2 || b.WindowS != 3600 || b.NextFreeAt != nil {
		t.Fatalf("fresh: %+v", st.WriteBudget)
	}
	last := writeUntilRefused(t, h)
	p := checkProblem(t, last, http.StatusTooManyRequests, "write_budget")
	if p.Setting != "work_mode" || p.Remaining == nil || *p.Remaining != 0 || p.NextFreeAt == nil {
		t.Fatalf("429 body: %+v", p)
	}
	if ra, err := strconv.Atoi(last.Header().Get("Retry-After")); err != nil || ra <= 0 || ra > 3600 {
		t.Fatalf("Retry-After %q", last.Header().Get("Retry-After"))
	}
	decode(t, h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil), &st)
	if b := st.WriteBudget["work_mode"]; b.Remaining != 0 || b.NextFreeAt == nil {
		t.Fatalf("spent: %+v", b)
	}
}

// writeUntilRefused flips the work mode until the budget refuses a write.
func writeUntilRefused(t *testing.T, h *harness) *httptest.ResponseRecorder {
	t.Helper()
	modes := []string{workModeFeedIn, workModeSelfUse}
	var last *httptest.ResponseRecorder
	for i := range 12 {
		last = h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: modes[i%2]})
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	return last
}
