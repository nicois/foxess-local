package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"

	"foxesslocal/internal/gateway"
)

// The installer settings endpoints (the Control page's numbered limits):
// each is one gateway command kind of single-register fn06 writes,
// proven by captured cloud writes on the verified profile (protocol
// profile §5). Fields use /state's names and units; amps go to the wire
// in 0.1 A.

// settingField is one request field: its gateway param, unit scale and
// range (in request units).
type settingField struct {
	json     string
	param    gateway.Param
	scale    float64 // request unit → wire (10 for amps)
	min, max float64
	enum     map[string]int // a string field's wire values (EPS mode)
}

// settingEndpoint is one endpoint: its command kind and fields.
type settingEndpoint struct {
	path   string
	kind   gateway.CommandKind
	label  string
	fields []settingField
}

// The installer schema's ranges, in request units (protocol profile §5).
const (
	deciAmpScale = 10    // amps → 0.1 A on the wire
	minGridAmps  = 2     // 41027 / 41028
	maxGridAmps  = 100   // 41027 / 41028
	maxBattAmps  = 50    // 41007 / 41008
	maxPct       = 100   // 41016
	maxPortW     = 65535 // 41200 / 41201
	epsUPS       = 3     // 41015
	epsEPS       = 2     // 41015
)

// settingEndpoints is the table.
var settingEndpoints = []settingEndpoint{
	{"/api/v1/current-limits", gateway.KindCurrentLimits, "current limits", []settingField{
		{json: "import_current_a", param: gateway.ParamImport, scale: deciAmpScale, min: minGridAmps, max: maxGridAmps},
		{json: "export_current_a", param: gateway.ParamExport, scale: deciAmpScale, min: minGridAmps, max: maxGridAmps},
	}},
	{"/api/v1/battery-currents", gateway.KindBatteryCurrents, "battery currents", []settingField{
		{json: "max_charge_current_a", param: gateway.ParamCharge, scale: deciAmpScale, min: 0, max: maxBattAmps},
		{json: "max_discharge_current_a", param: gateway.ParamDischarge, scale: deciAmpScale, min: 0, max: maxBattAmps},
	}},
	{"/api/v1/power-limit", gateway.KindPowerLimit, "power limit", []settingField{
		{json: "power_limit_pct", param: gateway.ParamPct, scale: 1, min: 0, max: maxPct},
	}},
	{"/api/v1/port-limits", gateway.KindPortLimits, "grid-port limits", []settingField{
		{json: "load_limit_w", param: gateway.ParamLoad, scale: 1, min: 0, max: maxPortW},
		{json: "generation_limit_w", param: gateway.ParamGeneration, scale: 1, min: 0, max: maxPortW},
	}},
	{"/api/v1/eps-mode", gateway.KindEPSMode, "EPS mode", []settingField{
		// "disabled" (1) is presumed and was never written on this
		// device: not offered
		{json: "eps_mode", param: gateway.ParamMode, enum: map[string]int{"ups": epsUPS, "eps": epsEPS}},
	}},
}

// inverterSettingRoutes registers the endpoints (installer).
func (s *Server) inverterSettingRoutes(mux *http.ServeMux) {
	for i := range settingEndpoints {
		ep := settingEndpoints[i]
		s.handle(mux, "PUT "+ep.path, s.protect(RoleInstaller, func(w http.ResponseWriter, r *http.Request, tk Token) {
			s.handleSettingWrite(w, r, tk, ep)
		}))
	}
}

// handleSettingWrite validates the body against the endpoint's fields
// (at least one, each in range, nothing unknown), then runs the command;
// the response reads back the fields as set.
func (s *Server) handleSettingWrite(w http.ResponseWriter, r *http.Request, tk Token, ep settingEndpoint) {
	var body map[string]json.RawMessage
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	params, set, p := parseSettingFields(ep, body)
	if p != nil {
		writeProblem(w, http.StatusBadRequest, *p)
		return
	}
	resp := s.runCommand(r, tk, gateway.Command{Kind: ep.kind, Params: params}, settingSummary(ep, set), set)
	s.writeCommand(w, resp)
}

// parseSettingFields returns the gateway params and the read-back body,
// or the first problem.
func parseSettingFields(ep settingEndpoint, body map[string]json.RawMessage) (map[gateway.Param]int, map[string]any, *problem) {
	if len(body) == 0 {
		return nil, nil, badFieldPtr("body", "at least one field is required")
	}
	params, set := map[gateway.Param]int{}, map[string]any{}
	for _, f := range ep.fields {
		raw, ok := body[f.json]
		if !ok {
			continue
		}
		delete(body, f.json)
		wire, shown, err := f.parse(raw)
		if err != nil {
			return nil, nil, badFieldPtr(f.json, err.Error())
		}
		params[f.param], set[f.json] = wire, shown
	}
	for k := range body {
		return nil, nil, badFieldPtr(k, "not a field of this setting")
	}
	return params, set, nil
}

// parse converts one value to wire units (and the value shown back).
func (f settingField) parse(raw json.RawMessage) (int, any, error) {
	if f.enum != nil {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return 0, nil, fmt.Errorf("must be one of %s", enumNames(f.enum))
		}
		wire, ok := f.enum[v]
		if !ok {
			return 0, nil, fmt.Errorf("must be one of %s", enumNames(f.enum))
		}
		return wire, v, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || v < f.min || v > f.max || math.IsNaN(v) {
		return 0, nil, fmt.Errorf("must be a number from %g to %g", f.min, f.max)
	}
	wire := int(math.Round(v * f.scale))
	return wire, float64(wire) / f.scale, nil
}

func enumNames(m map[string]int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// settingSummary is the command's audit summary.
func settingSummary(ep settingEndpoint, set map[string]any) string {
	parts := make([]string, 0, len(set))
	for _, f := range ep.fields {
		if v, ok := set[f.json]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", f.json, v))
		}
	}
	return ep.label + " " + strings.Join(parts, " ")
}
