package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
)

// The register read's request fields.
const (
	jsonFieldAddr  = "addr"
	jsonFieldCount = "count"
)

// TestPostRegistersRead: the installer probe reads a register block
// through the gateway (fn04 here) and returns the device's values in
// address order — the live-value decode's instrument.
func TestPostRegistersRead(t *testing.T) {
	h := newHarness(t)
	h.inv.mu.Lock()
	h.inv.cur[31020] = 5523
	h.inv.cur[31021] = 0xFFF6
	h.inv.mu.Unlock()
	rr := h.do(t, httpPost, "/api/v1/registers/read", RoleInstaller, map[string]int{"fn": 4, jsonFieldAddr: 31020, jsonFieldCount: 2})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /registers/read: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body registerReadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Fn != 4 || body.Addr != 31020 || len(body.Registers) != 2 {
		t.Fatalf("body: %+v", body)
	}
	if body.Registers[0] != (registerValue{Addr: 31020, Value: 5523}) ||
		body.Registers[1] != (registerValue{Addr: 31021, Value: 0xFFF6}) {
		t.Fatalf("registers: %+v", body.Registers)
	}
	frs := h.inv.injected()
	if len(frs) != 1 {
		t.Fatalf("injected %d frames, want 1", len(frs))
	}
	if mb := checkMb(t, frs[0]); mb.Fn != 0x04 || mb.Addr != 31020 || mb.Count != 2 {
		t.Fatalf("frame: fn=%#x addr=%d count=%d", mb.Fn, mb.Addr, mb.Count)
	}
}

// TestPostRegistersReadInstallerOnly: the probe reaches arbitrary
// registers on the live device — installer role only.
func TestPostRegistersReadInstallerOnly(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPost, "/api/v1/registers/read", RoleExpert, map[string]int{"fn": 3, jsonFieldAddr: 31000, jsonFieldCount: 1})
	checkProblem(t, rr, http.StatusForbidden, "insufficient_role")
}

// TestPostRegistersReadInvalid: the gateway's read validation surfaces
// as 400 before anything reaches the wire.
func TestPostRegistersReadInvalid(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPost, "/api/v1/registers/read", RoleInstaller, map[string]int{"fn": 6, jsonFieldAddr: 31000, jsonFieldCount: 1})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("fn 6: got %d, want 400", rr.Code)
	}
	if n := len(h.inv.injected()); n != 0 {
		t.Fatalf("an invalid read injected %d frames", n)
	}
}

// TestCommandBehindPollSucceeds: an operator command arriving while a
// poll read is in flight queues behind it and completes — never a 409
// or a false "inverter offline".
func TestCommandBehindPollSucceeds(t *testing.T) {
	h := newHarness(t)
	h.inv.setAutoAck(false)
	pollID, err := h.gw.Submit(t.Context(), gateway.Command{
		Kind: gateway.KindRead, Params: map[gateway.Param]int{gateway.ParamFn: 3, gateway.ParamAddr: 31000, gateway.ParamCount: 1},
		Origin: gateway.OriginPoll,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.inv.setAutoAck(true)
	code := make(chan int, 1)
	go func() {
		rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})
		code <- rr.Code
	}()
	// wait until the work-mode command is queued behind the poll
	deadline := time.Now().Add(5 * time.Second)
	for len(h.gw.Pending()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := h.gw.Ack(pollID, frame.Frame{Device: invLead, Fn: 0x10, Payload: []byte{0x01, 0x03, 0x02, 0x00, 0x01}}); err != nil {
		t.Fatal(err)
	}
	if c := <-code; c != http.StatusOK {
		t.Fatalf("PUT /work-mode behind a poll: got %d, want 200", c)
	}
}
