package gateway

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func readCmd(fn, addr, count int) Command {
	return Command{Kind: KindRead, Params: map[Param]int{ParamFn: fn, keyAddr: addr, ParamCount: count}, Origin: "probe"}
}

// TestValidateRead: a read names its function (03 holding / 04 input),
// a 16-bit start address and 1..125 registers (the Modbus PDU limit).
func TestValidateRead(t *testing.T) {
	ok := []Command{readCmd(3, 31000, 29), readCmd(4, 11000, 125), readCmd(3, 0, 1), readCmd(3, 65535, 1)}
	for _, c := range ok {
		if err := validate(c); err != nil {
			t.Errorf("validate %v: %v", c.Params, err)
		}
	}
	bad := map[string]Command{
		"fn 6":      readCmd(6, 31000, 1),
		"count 0":   readCmd(3, 31000, 0),
		"count 126": readCmd(3, 31000, 126),
		"addr -1":   readCmd(3, -1, 1),
		"addr 64k":  readCmd(3, 65536, 1),
		"wraps":     readCmd(3, 65530, 10),
	}
	for name, c := range bad {
		if err := validate(c); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%s: got %v, want ErrOutOfRange", name, err)
		}
	}
	missing := Command{Kind: KindRead, Params: map[Param]int{ParamFn: 3, keyAddr: 1}}
	if err := validate(missing); !errors.Is(err, ErrMissingParam) {
		t.Errorf("missing count: got %v, want ErrMissingParam", err)
	}
}

// TestReadRoundTrip: the read injects one fn04 request with the right
// PDU and completes with the response's registers — no write phase.
func TestReadRoundTrip(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")
	id, err := gw.Submit(t.Context(), readCmd(4, 31000, 3))
	if err != nil {
		t.Fatal(err)
	}
	calls := inj.injected()
	if len(calls) != 1 {
		t.Fatalf("injected %d frames, want 1", len(calls))
	}
	wantPDU := []byte{0x01, 0x04, 0x79, 0x18, 0x00, 0x03}
	if !bytes.Contains(calls[0].raw, wantPDU) {
		t.Fatalf("frame %x does not carry PDU %x", calls[0].raw, wantPDU)
	}
	resp := []byte{0x01, 0x04, 0x06, 0x00, 0x0a, 0xff, 0xfe, 0x12, 0x34}
	if err := ack(gw, id, resp); err != nil {
		t.Fatal(err)
	}
	res, err := gw.Wait(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAcked {
		t.Fatalf("status %s (%s), want acked", res.Status, res.Detail)
	}
	want := []RegisterValue{{31000, 0x000a}, {31001, 0xfffe}, {31002, 0x1234}}
	if len(res.Registers) != len(want) {
		t.Fatalf("registers %v, want %v", res.Registers, want)
	}
	for i := range want {
		if res.Registers[i] != want[i] {
			t.Errorf("register %d: %v, want %v", i, res.Registers[i], want[i])
		}
	}
	if len(inj.injected()) != 1 {
		t.Errorf("a read must inject no write phase")
	}
}

// TestReadWrongFunctionFails: a response for the other function code is
// not this read's answer.
func TestReadWrongFunctionFails(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id, err := gw.Submit(t.Context(), readCmd(4, 31000, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := ack(gw, id, []byte{0x01, 0x03, 0x02, 0x00, 0x01}); !errors.Is(err, ErrEchoMismatch) {
		t.Fatalf("got %v, want ErrEchoMismatch", err)
	}
}

// TestReadExceptionFails: a Modbus exception (fn|0x80, code) fails the
// read with the code in the detail — the probe's "register not
// supported" answer, never a hang until timeout.
func TestReadExceptionFails(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id, err := gw.Submit(t.Context(), readCmd(3, 30016, 1))
	if err != nil {
		t.Fatal(err)
	}
	_ = ack(gw, id, []byte{0x01, 0x83, 0x02})
	res, err := gw.Wait(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status %s, want failed", res.Status)
	}
	if !bytes.Contains([]byte(res.Detail), []byte("modbus exception 2")) {
		t.Errorf("detail %q does not name the exception code", res.Detail)
	}
	_ = time.Second
}
