package api

import (
	"fmt"
	"net/http"

	"foxesslocal/internal/gateway"
)

// registerReadRequest is POST /registers/read: one Modbus read block.
type registerReadRequest struct {
	Fn    *int `json:"fn"`    // 3 holding / 4 input
	Addr  *int `json:"addr"`  // start register
	Count *int `json:"count"` // 1..125
}

// registerValue is one register in a read response.
type registerValue struct {
	Addr  int `json:"addr"`
	Value int `json:"value"`
}

// registerReadResponse is the device's answer, in address order.
type registerReadResponse struct {
	Fn        int             `json:"fn"`
	Addr      int             `json:"addr"`
	Registers []registerValue `json:"registers"`
}

// handleRegistersRead is POST /registers/read (installer): a raw
// register read through the gateway — the recon probe for live-value
// registers. Reads only; it writes nothing to the device (C-003: the
// gateway is still the one path, validated and audited).
func (s *Server) handleRegistersRead(w http.ResponseWriter, r *http.Request, tk Token) {
	var body registerReadRequest
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	for _, f := range []struct {
		name string
		v    *int
	}{{"fn", body.Fn}, {"addr", body.Addr}, {"count", body.Count}} {
		if f.v == nil {
			writeProblem(w, http.StatusBadRequest, badField(f.name, "required"))
			return
		}
	}
	fn, addr, count := *body.Fn, *body.Addr, *body.Count
	resp := s.runCommand(r, tk, gateway.Command{
		Kind:   gateway.KindRead,
		Params: map[gateway.Param]int{gateway.ParamFn: fn, gateway.ParamAddr: addr, gateway.ParamCount: count},
	}, fmt.Sprintf("register read fn%02d @%d x%d", fn, addr, count),
		func(res gateway.Result) any {
			out := registerReadResponse{Fn: fn, Addr: addr, Registers: make([]registerValue, 0, len(res.Registers))}
			for _, rv := range res.Registers {
				out.Registers = append(out.Registers, registerValue{Addr: int(rv.Addr), Value: int(rv.Value)})
			}
			return out
		})
	s.writeCommand(w, resp)
}
