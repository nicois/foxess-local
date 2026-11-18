package sim

import (
	"strconv"

	"foxesslocal/internal/store"
)

// kw is the watts-per-kW scale of the real/query values.
const kw = 1000.0

// voltScale is the snap61 DC-bus voltage scale (0.01 V units).
const voltScale = 100.0

// variableSource maps one decoded state to a real/query value; false
// means "no data" (the variable is omitted, reference-simulator parity).
type variableSource func(st store.State) (float64, bool)

// teleOf wraps a tele accessor: available once any tele has been seen.
func teleOf(f func(st store.State) float64) variableSource {
	return func(st store.State) (float64, bool) {
		if st.TeleTS.IsZero() {
			return 0, false
		}
		return f(st), true
	}
}

// constOf is a variable with a fixed value (single-MPPT aliases).
func constOf(v float64) variableSource {
	return func(store.State) (float64, bool) { return v, true }
}

func kwOf(v int32) float64 {
	return float64(v) / kw
}

// variables maps the integration's POLLED_VARIABLES (const.py) onto the
// decoded state. Backed: the 150 B tele (store.State) and the snap61 DC
// bus. Dropped (no decoded backing — reference-simulator parity):
// batTemperature, batCurrent, ambientTemperation, invTemperation, the
// lifetime-energy group, and the RF/eps fields.
var variables = map[string]variableSource{
	"SoC":                  teleOf(func(st store.State) float64 { return float64(st.Soc) }),
	"pvPower":              teleOf(func(st store.State) float64 { return kwOf(st.Sol) }),
	"pv1Power":             teleOf(func(st store.State) float64 { return kwOf(st.Sol) }),
	"pv2Power":             constOf(0), // single MPPT: no second PV input
	"loadsPower":           teleOf(func(st store.State) float64 { return kwOf(st.Haus) }),
	"batChargePower":       teleOf(func(st store.State) float64 { return max(kwOf(st.Batt), 0) }),
	"batDischargePower":    teleOf(func(st store.State) float64 { return max(-kwOf(st.Batt), 0) }),
	"gridConsumptionPower": teleOf(func(st store.State) float64 { return max(kwOf(st.Grid), 0) }),
	"feedinPower":          teleOf(func(st store.State) float64 { return max(-kwOf(st.Grid), 0) }),
	"generationPower":      teleOf(func(st store.State) float64 { return kwOf(st.Sol) }),
	"meterPower":           teleOf(func(st store.State) float64 { return kwOf(st.Grid) }),
	"meterPower2":          constOf(0), // the additional-PV default variable
	"batVolt": func(st store.State) (float64, bool) {
		if len(st.BusVoltage) == 0 {
			return 0, false
		}
		sum := 0
		for _, v := range st.BusVoltage {
			sum += v
		}
		return float64(sum) / voltScale / float64(len(st.BusVoltage)), true
	},
}

// WS message shapes (the reference simulator's get_ws_message; the
// integration's map_ws_to_coordinator).
type wsPowerVal struct {
	Value string `json:"value"`
	Unit  string `json:"unit"`
}

type wsPowerNode struct {
	Power wsPowerVal `json:"power"`
}

type wsBatSn struct {
	BatSn string `json:"batSn"`
	Soc   int    `json:"soc"`
}

type wsBatNode struct {
	Soc       int        `json:"soc"`
	Charge    string     `json:"charge"`
	Power     wsPowerVal `json:"power"`
	BatteryID string     `json:"batteryId"`
	Multiple  []wsBatSn  `json:"multipleBatterySoc"`
}

type wsGridNode struct {
	Power      wsPowerVal `json:"power"`
	GridStatus int        `json:"gridStatus"`
}

type wsNode struct {
	Bat   wsBatNode   `json:"bat"`
	Solar wsPowerNode `json:"solar"`
	Load  wsPowerNode `json:"load"`
	Grid  wsGridNode  `json:"grid"`
}

type wsResult struct {
	TimeDiff int     `json:"timeDiff"`
	Node     *wsNode `json:"node"`
}

// wsMessage is one wsmaitian text frame.
type wsMessage struct {
	Errno  int      `json:"errno"`
	Msg    string   `json:"msg"`
	Result wsResult `json:"result"`
}

const (
	wsUnitW   = "W"
	chargeOn  = "1"
	chargeOff = "0"
)

// wsGridStatusExport / Import are the cloud grid-status codes the client
// derives the balance from (the panel identity holds exactly in the tele).
const (
	wsGridStatusExport = 2
	wsGridStatusImport = 3
)

// wsMessageFor builds the real-time message for a state snapshot.
func (s *Server) wsMessageFor(st store.State) wsMessage {
	timeDiff := staleTimeDiff
	if !st.TeleTS.IsZero() {
		timeDiff = int(s.clock().Sub(st.TeleTS).Seconds())
	}
	id := s.deviceID()
	soc := st.Soc
	absBatt := abs32(st.Batt)
	absGrid := abs32(st.Grid)
	node := &wsNode{
		Bat: wsBatNode{
			Soc:       soc,
			Charge:    chargeOff,
			Power:     wsPowerVal{Value: strconv.Itoa(absBatt), Unit: wsUnitW},
			BatteryID: "sim-battery-" + id.SN,
		},
		Solar: wsPowerNode{Power: wsPowerVal{Value: strconv.Itoa(abs32(st.Sol)), Unit: wsUnitW}},
		Load:  wsPowerNode{Power: wsPowerVal{Value: strconv.Itoa(abs32(st.Haus)), Unit: wsUnitW}},
		Grid: wsGridNode{
			Power:      wsPowerVal{Value: strconv.Itoa(absGrid), Unit: wsUnitW},
			GridStatus: wsGridStatusExport,
		},
	}
	if st.Batt > 0 {
		node.Bat.Charge = chargeOn
	}
	if st.Grid > 0 {
		node.Grid.GridStatus = wsGridStatusImport
	}
	for _, sn := range id.BatterySNs {
		node.Bat.Multiple = append(node.Bat.Multiple, wsBatSn{BatSn: sn, Soc: soc})
	}
	return wsMessage{Errno: errnoSuccess, Msg: "success", Result: wsResult{TimeDiff: timeDiff, Node: node}}
}

// abs32 is the bounded absolute value of a tele int32.
func abs32(v int32) int {
	if v < 0 {
		return int(-v)
	}
	return int(v)
}
