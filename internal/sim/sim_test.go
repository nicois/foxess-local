package sim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

// testBase is the seed base time for the endpoint tests (the WS tests use
// now-relative bases so the feed reads fresh).
var testBase = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// wantWebToken is the configured web-session token of the test servers.
const wantWebToken = "test-web-token"

// TestAuth401 pins the auth matrix: missing/wrong api key is HTTP 401 on
// /op/v0 (the integration treats non-2xx as fatal); the api_key header is
// an accepted alias; the WS upgrade and the dew detail require the web
// token.
func TestAuth401(t *testing.T) {
	st := openTestStore(t)
	ts := newTestServer(t, st, &fakeGW{})

	req := func(method, target, header, key string) int {
		r := httptest.NewRequest(method, target, nil)
		if key != "" {
			r.Header.Set(header, key)
		}
		rr := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(rr, r)
		return rr.Code
	}

	if code := req("POST", "/op/v0/plant/list", "token", ""); code != http.StatusUnauthorized {
		t.Errorf("missing token: got %d, want 401", code)
	}
	if code := req("POST", "/op/v0/plant/list", "token", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
	if code := req("POST", "/op/v0/plant/list", "api_key", testAPIKey); code != http.StatusOK {
		t.Errorf("api_key header: got %d, want 200", code)
	}
	if code := req("GET", "/dew/v0/wsmaitian?plantId=x&token=wrong", "", ""); code != http.StatusUnauthorized {
		t.Errorf("ws wrong token: got %d, want 401", code)
	}
	if code := req("GET", "/dew/v0/device/detail?id=1&category=battery", "token", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("dew wrong token: got %d, want 401", code)
	}
}

// TestLoginPlantAndDeviceList pins the discovery sequence: login issues
// the static web token, plant/list returns the configured record, and
// device/list returns the stored SN (empty store → empty data).
func TestLoginPlantAndDeviceList(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase)
	ts := newTestServer(t, st, &fakeGW{})

	rr := ts.do(t, "POST", "/basic/v0/user/login",
		map[string]any{"user": "u", "password": "p", "type": 1, "verification": 1})
	var login struct {
		Errno  int `json:"errno"`
		Result struct {
			Token string `json:"token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &login); err != nil {
		t.Fatalf("login: %v", err)
	}
	if login.Errno != errnoSuccess || login.Result.Token != wantWebToken {
		t.Errorf("login: errno %d token %q (want %q)", login.Errno, login.Result.Token, wantWebToken)
	}

	rr = ts.do(t, "POST", "/op/v0/plant/list", map[string]any{keyCurrentPage: 1, keyPageSize: 10})
	var plant struct {
		Errno  int `json:"errno"`
		Result struct {
			Data []struct {
				StationID string `json:"stationID"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &plant); err != nil {
		t.Fatalf("plant/list: %v", err)
	}
	if plant.Errno != errnoSuccess || len(plant.Result.Data) != 1 || plant.Result.Data[0].StationID != testStation {
		t.Errorf("plant/list: errno %d data %+v", plant.Errno, plant.Result.Data)
	}

	rr = ts.do(t, "POST", "/op/v0/device/list", map[string]any{keyCurrentPage: 1, keyPageSize: 10})
	var dev struct {
		Errno  int `json:"errno"`
		Result struct {
			Data []struct {
				DeviceSN string `json:"deviceSN"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatalf("device/list: %v", err)
	}
	if dev.Errno != errnoSuccess || len(dev.Result.Data) != 1 || dev.Result.Data[0].DeviceSN != testSN {
		t.Errorf("device/list: errno %d data %+v", dev.Errno, dev.Result.Data)
	}

	// Empty store: no SN observed yet → empty data (the integration then
	// reports no devices — the pre-first-burst state).
	stEmpty := openTestStore(t)
	tsEmpty := newTestServer(t, stEmpty, &fakeGW{})
	rr = tsEmpty.do(t, "POST", "/op/v0/device/list", map[string]any{keyCurrentPage: 1, keyPageSize: 10})
	var devEmpty struct {
		Errno  int `json:"errno"`
		Result struct {
			Data []struct {
				DeviceSN string `json:"deviceSN"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &devEmpty); err != nil {
		t.Fatalf("device/list empty: %v", err)
	}
	if devEmpty.Errno != errnoSuccess || len(devEmpty.Result.Data) != 0 {
		t.Errorf("device/list empty: errno %d data %+v", devEmpty.Errno, devEmpty.Result.Data)
	}
}

// TestDeviceDetail pins the detail record: the configured capacity (the
// integration requires > 0) and the stored model/SN/firmware.
func TestDeviceDetail(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase)
	ts := newTestServer(t, st, &fakeGW{})

	rr := ts.do(t, "GET", "/op/v0/device/detail?sn="+testSN, nil)
	var det struct {
		Errno  int `json:"errno"`
		Result struct {
			Capacity   int      `json:"capacity"`
			DeviceType string   `json:"deviceType"`
			DeviceSN   string   `json:"deviceSN"`
			Firmware   []string `json:"firmware"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &det); err != nil {
		t.Fatalf("device/detail: %v", err)
	}
	if det.Errno != errnoSuccess {
		t.Fatalf("device/detail errno: %d", det.Errno)
	}
	if det.Result.Capacity != testCapacity {
		t.Errorf("capacity: got %d, want %d", det.Result.Capacity, testCapacity)
	}
	if det.Result.DeviceType != "KH10" || det.Result.DeviceSN != testSN {
		t.Errorf("detail: type %q sn %q", det.Result.DeviceType, det.Result.DeviceSN)
	}
	wantFW := []string{"1.73", "1.03", "1.71", "0.00"}
	if len(det.Result.Firmware) != len(wantFW) {
		t.Fatalf("firmware: got %v", det.Result.Firmware)
	}
	for i := range wantFW {
		if det.Result.Firmware[i] != wantFW[i] {
			t.Errorf("firmware[%d]: got %q, want %q", i, det.Result.Firmware[i], wantFW[i])
		}
	}
}

// TestRealQueryVariables pins the integration's polled variable map:
// backed variables carry the tele/snap61 values, single-MPPT and
// unknown variables are absent (reference-simulator parity).
func TestRealQueryVariables(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase)
	ts := newTestServer(t, st, &fakeGW{})

	// The coordinator's POLLED_VARIABLES (const.py) plus the additional-PV
	// default.
	vars := []string{
		"SoC", "batChargePower", "batDischargePower", "loadsPower", "pvPower",
		"batTemperature", "gridConsumptionPower", "feedinPower",
		"generationPower", "batVolt", "batCurrent", "pv1Power", "pv2Power",
		"ambientTemperation", "invTemperation",
		"feedin", "gridConsumption", "generation",
		"chargeEnergyToTal", "dischargeEnergyToTal", "loads", "energyThroughput",
		"meterPower", "RVolt", "RCurrent", "RFreq", "epsPower", "meterPower2",
	}
	rr := ts.do(t, "POST", "/op/v0/device/real/query",
		map[string]any{"sn": testSN, "variables": vars})
	var q struct {
		Errno  int `json:"errno"`
		Result []struct {
			Datas []struct {
				Variable string  `json:"variable"`
				Value    float64 `json:"value"`
			} `json:"datas"`
			DeviceSN string `json:"deviceSN"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &q); err != nil {
		t.Fatalf("real/query: %v", err)
	}
	if q.Errno != errnoSuccess || len(q.Result) != 1 || q.Result[0].DeviceSN != testSN {
		t.Fatalf("real/query: errno %d result %+v", q.Errno, q.Result)
	}
	got := map[string]float64{}
	for _, d := range q.Result[0].Datas {
		got[d.Variable] = d.Value
	}

	// Seeded tele: Sol 2500, Grid -800 (export), Haus 1200, Batt +1500
	// (charging), Soc 42; snap61 voltages 52000/53000 (0.01 V).
	want := map[string]float64{
		"SoC":                  42,
		"batChargePower":       1.5,
		"batDischargePower":    0,
		"loadsPower":           1.2,
		"pvPower":              2.5,
		"pv1Power":             2.5,
		"pv2Power":             0,
		"gridConsumptionPower": 0,
		"feedinPower":          0.8,
		"generationPower":      2.5,
		"batVolt":              525.0,
		"meterPower":           -0.8,
		"meterPower2":          0,
	}
	for name, v := range want {
		if g, ok := got[name]; !ok {
			t.Errorf("%s: missing (reference simulator drops unknowns)", name)
		} else if g != v {
			t.Errorf("%s: got %v, want %v", name, g, v)
		}
	}
	for _, absent := range []string{
		"batTemperature", "batCurrent", "ambientTemperation", "invTemperation",
		"feedin", "gridConsumption", "generation", "chargeEnergyToTal",
		"dischargeEnergyToTal", "loads", "energyThroughput",
		"RVolt", "RCurrent", "RFreq", "epsPower",
	} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s: must be absent (no decoded backing)", absent)
		}
	}
}

// checkSocEnvelope decodes a soc/get envelope into (errno, minSoc,
// minSocOnGrid).
func checkSocEnvelope(t *testing.T, body []byte) (int, int, any) {
	t.Helper()
	var b struct {
		Errno  int `json:"errno"`
		Result struct {
			MinSoc       int `json:"minSoc"`
			MinSocOnGrid int `json:"minSocOnGrid"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("soc envelope: %v", err)
	}
	return b.Errno, b.Result.MinSoc, any(b.Result.MinSocOnGrid)
}

// assertOneAlertEvent pins exactly one alert, with a detail.
func assertOneAlertEvent(t *testing.T, events chan string) {
	t.Helper()
	select {
	case detail := <-events:
		if detail == "" {
			t.Error("an alert without a detail")
		}
	default:
		t.Error("no bus event")
	}
	select {
	case ev := <-events:
		t.Errorf("extra bus event: %+v", ev)
	default:
	}
}

// TestSocSetLocalOnly pins the soc/set decision: the pair is sim-local
// (no gateway command of any kind), one bus alert, cloud envelope.
func TestSocSetLocalOnly(t *testing.T) {
	st := openTestStore(t)
	gw := &fakeGW{}
	ts := newTestServer(t, st, gw)

	// Config defaults (zero → 10 / 11).
	rr := ts.do(t, "GET", "/op/v0/device/battery/soc/get?sn="+testSN, nil)
	errno, minSoc, minSocOnGrid := checkSocEnvelope(t, rr.Body.Bytes())
	if errno != errnoSuccess || minSoc != defaultMinSoc || minSocOnGrid != defaultMinSocOnGrid {
		t.Errorf("defaults: errno %d %v / %v", errno, minSoc, minSocOnGrid)
	}

	// Set a new pair.
	rr = ts.do(t, "POST", "/op/v0/device/battery/soc/set",
		map[string]any{"sn": testSN, "minSoc": 12, keyMinSocGrid: 15})
	var set struct {
		Errno  int  `json:"errno"`
		Result any  `json:"result"`
		Msg    bool `json:"-"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &set); err != nil {
		t.Fatalf("soc/set: %v", err)
	}
	if set.Errno != errnoSuccess || set.Result != nil {
		t.Errorf("soc/set: errno %d result %v (want 0 / null)", set.Errno, set.Result)
	}

	rr = ts.do(t, "GET", "/op/v0/device/battery/soc/get?sn="+testSN, nil)
	_, minSoc, minSocOnGrid = checkSocEnvelope(t, rr.Body.Bytes())
	if minSoc != 12 || minSocOnGrid != 15 {
		t.Errorf("round trip: %v / %v (want 12 / 15)", minSoc, minSocOnGrid)
	}

	// Exactly one bus event, kind system, type alert.
	assertOneAlertEvent(t, ts.events)

	// No gateway command of any kind.
	if n := len(gw.cmds()); n != 0 {
		t.Errorf("gateway submissions: %d (soc/set is sim-local)", n)
	}

	// Out of range: rejected, pair unchanged.
	rr = ts.do(t, "POST", "/op/v0/device/battery/soc/set",
		map[string]any{"sn": testSN, "minSoc": 101, keyMinSocGrid: 15})
	var bad struct {
		Errno int `json:"errno"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &bad); err != nil {
		t.Fatalf("soc/set bad: %v", err)
	}
	if bad.Errno == errnoSuccess {
		t.Error("soc/set: 101 must be rejected")
	}
	rr = ts.do(t, "GET", "/op/v0/device/battery/soc/get?sn="+testSN, nil)
	_, minSoc, minSocOnGrid = checkSocEnvelope(t, rr.Body.Bytes())
	if minSoc != 12 || minSocOnGrid != 15 {
		t.Errorf("pair after rejected set: %v / %v", minSoc, minSocOnGrid)
	}
}

// schedGroup is one 48010 group as scheduler/get returns it (the
// integration's name shape).
type schedGroup struct {
	Enable       int    `json:"enable"`
	StartHour    int    `json:"startHour"`
	StartMinute  int    `json:"startMinute"`
	EndHour      int    `json:"endHour"`
	EndMinute    int    `json:"endMinute"`
	WorkMode     string `json:"workMode"`
	MinSocOnGrid int    `json:"minSocOnGrid"`
	FdSoc        int    `json:"fdSoc"`
	FdPwr        int    `json:"fdPwr"`
}

// checkSchedGroupShape pins the seeded schedule: slot 1 full-day SelfUse,
// slot 2 unused (Invalid), slot 3 evening ForceDischarge.
func checkSchedGroupShape(t *testing.T, groups []schedGroup) {
	t.Helper()
	want := map[int]schedGroup{
		0: {
			Enable: 1, EndHour: 23, EndMinute: 59,
			WorkMode: "SelfUse", MinSocOnGrid: 10, FdSoc: 30, FdPwr: 5000,
		},
		1: {WorkMode: "Invalid"},
		2: {
			Enable: 1, StartHour: 18, EndHour: 20, WorkMode: "ForceDischarge",
			MinSocOnGrid: 17, FdSoc: 20, FdPwr: 100,
		},
	}
	for i, w := range want {
		if groups[i] != w {
			t.Errorf("group %d: %+v (want %+v)", i+1, groups[i], w)
		}
	}
}

// TestSchedulerGet pins the 48010 table as the cloud shape: eight groups,
// enable derived from the slots, wire work-mode enums decoded to the
// integration's names, properties present (cloud shape).
func TestSchedulerGet(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase)
	ts := newTestServer(t, st, &fakeGW{})

	rr := ts.do(t, "POST", "/op/v0/device/scheduler/get", map[string]any{keyDeviceSN: testSN})
	var sg struct {
		Errno  int `json:"errno"`
		Result struct {
			Enable     int          `json:"enable"`
			Groups     []schedGroup `json:"groups"`
			Properties any          `json:"properties"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sg); err != nil {
		t.Fatalf("scheduler/get: %v", err)
	}
	if sg.Errno != errnoSuccess || sg.Result.Properties == nil {
		t.Fatalf("scheduler/get: errno %d properties %v", sg.Errno, sg.Result.Properties)
	}
	if sg.Result.Enable != 1 {
		t.Errorf("enable: got %d, want 1", sg.Result.Enable)
	}
	if len(sg.Result.Groups) != groupBound {
		t.Fatalf("groups: got %d, want %d", len(sg.Result.Groups), groupBound)
	}
	checkSchedGroupShape(t, sg.Result.Groups)

	// No stored schedule → result null (the integration normalises it).
	stEmpty := openTestStore(t)
	tsEmpty := newTestServer(t, stEmpty, &fakeGW{})
	rr = tsEmpty.do(t, "POST", "/op/v0/device/scheduler/get", map[string]any{keyDeviceSN: testSN})
	var sgEmpty struct {
		Errno  int `json:"errno"`
		Result any `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sgEmpty); err != nil {
		t.Fatalf("scheduler/get empty: %v", err)
	}
	if sgEmpty.Errno != errnoSuccess || sgEmpty.Result != nil {
		t.Errorf("scheduler/get empty: errno %d result %v (want 0 / null)", sgEmpty.Errno, sgEmpty.Result)
	}
}

// checkScheduleParams pins the gateway's schedule params: the enabled
// flag, slot 1's packed window / wire work-mode / preserved maxSoC, and
// the zeroed disabled slots (no fields but _en).
func checkScheduleParams(t *testing.T, p map[gateway.Param]int) {
	t.Helper()
	if p[gateway.ParamEnabled] != 1 {
		t.Errorf("enabled: %d", p[gateway.ParamEnabled])
	}
	if p[gateway.SlotParam(1, gateway.SlotEnabled)] != 1 || p[gateway.SlotParam(1, gateway.SlotStart)] != 0 || p[gateway.SlotParam(1, gateway.SlotEnd)] != 23<<8|59 {
		t.Errorf("slot 1 window: en %d start %d end %d", p[gateway.SlotParam(1, gateway.SlotEnabled)], p[gateway.SlotParam(1, gateway.SlotStart)], p[gateway.SlotParam(1, gateway.SlotEnd)])
	}
	if p[gateway.SlotParam(1, gateway.SlotWorkMode)] != 1 {
		t.Errorf("slot 1 workmode: %d (want wire enum 1 SelfUse)", p[gateway.SlotParam(1, gateway.SlotWorkMode)])
	}
	if p[gateway.SlotParam(1, gateway.SlotMaxSoc)] != 80 {
		t.Errorf("slot 1 maxsoc: %d (want 80, preserved from the last push)", p[gateway.SlotParam(1, gateway.SlotMaxSoc)])
	}
	if p[gateway.SlotParam(1, gateway.SlotMinSoc)] != 11 || p[gateway.SlotParam(1, gateway.SlotFdSoc)] != 11 || p[gateway.SlotParam(1, gateway.SlotFdPwr)] != 10500 {
		t.Errorf("slot 1 soc/pwr: %d / %d / %d", p[gateway.SlotParam(1, gateway.SlotMinSoc)], p[gateway.SlotParam(1, gateway.SlotFdSoc)], p[gateway.SlotParam(1, gateway.SlotFdPwr)])
	}
	for k := 2; k <= groupBound; k++ {
		if p[gateway.SlotParam(k, gateway.SlotEnabled)] != 0 {
			t.Errorf("slot %d en: %d (want 0)", k, p[gateway.SlotParam(k, gateway.SlotEnabled)])
		}
		if _, ok := p[gateway.SlotParam(k, gateway.SlotMaxSoc)]; ok {
			t.Errorf("slot %d: disabled slots carry no fields but _en", k)
		}
	}
}

// enableSchedule POSTs scheduler/enable on a fresh test server with a
// scripted gateway and returns the errno envelope plus the submissions.
func enableSchedule(t *testing.T, gw *fakeGW, groups []map[string]any) (int, []gateway.Command) {
	t.Helper()
	ts := newTestServer(t, openTestStore(t), gw)
	rr := ts.do(t, "POST", "/op/v0/device/scheduler/enable",
		map[string]any{keyDeviceSN: testSN, keyGroups: groups})
	env := decodeEnvelope(t, rr.Body.Bytes())
	return env.Errno, gw.cmds()
}

// TestSchedulerEnableSubmitsSchedule pins the group→gateway mapping:
// KindSchedule with packed HHMM, the wire work-mode enum, the preserved
// maxSoC, zeroed extra slots — and the Wait verdict mapped to the errno
// envelope.
func TestSchedulerEnableSubmitsSchedule(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase) // slot 1 maxSoC 80 observed
	gw := &fakeGW{}
	ts := newTestServer(t, st, gw)

	// The integration's set_work_mode shape (single full-day group).
	groups := []map[string]any{{
		"enable": 1, "startHour": 0, "startMinute": 0, "endHour": 23, "endMinute": 59,
		"workMode": "SelfUse", keyMinSocGrid: 11, "fdSoc": 11, "fdPwr": 10500,
	}}
	rr := ts.do(t, "POST", "/op/v0/device/scheduler/enable",
		map[string]any{keyDeviceSN: testSN, keyGroups: groups})
	var en struct {
		Errno  int `json:"errno"`
		Result any `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &en); err != nil {
		t.Fatalf("scheduler/enable: %v", err)
	}
	if en.Errno != errnoSuccess || en.Result != nil {
		t.Fatalf("scheduler/enable: errno %d result %v", en.Errno, en.Result)
	}

	cmds := gw.cmds()
	if len(cmds) != 1 {
		t.Fatalf("submissions: %d, want 1", len(cmds))
	}
	c := cmds[0]
	if c.Kind != gateway.KindSchedule {
		t.Fatalf("kind: %q", c.Kind)
	}
	checkScheduleParams(t, c.Params)

	// No stored schedule → maxSoC default 100.
	gw2 := &fakeGW{}
	if errno, cmds := enableSchedule(t, gw2, groups); errno != errnoSuccess {
		t.Fatalf("scheduler/enable (no store): errno %d", errno)
	} else if p2 := cmds[0].Params; p2[gateway.SlotParam(1, gateway.SlotMaxSoc)] != 100 {
		t.Errorf("slot 1 maxsoc default: %d (want 100)", p2[gateway.SlotParam(1, gateway.SlotMaxSoc)])
	}

	// Gateway failure → non-success errno.
	if errno, _ := enableSchedule(t,
		&fakeGW{status: gateway.StatusFailed, detail: "echo mismatch"}, groups); errno == errnoSuccess {
		t.Error("scheduler/enable: failed command must be an errno envelope")
	}

	// Submit error (a command already in flight) → non-success errno.
	if errno, _ := enableSchedule(t, &fakeGW{submitErr: gateway.ErrInFlight}, groups); errno == errnoSuccess {
		t.Error("scheduler/enable: ErrInFlight must be an errno envelope")
	}

	// Unknown work-mode string → non-success errno (no submission).
	badGroups := []map[string]any{{
		"enable": 1, "startHour": 0, "startMinute": 0, "endHour": 23, "endMinute": 59,
		"workMode": "Turbo", keyMinSocGrid: 11, "fdSoc": 11, "fdPwr": 10500,
	}}
	gwBad := &fakeGW{}
	if errno, _ := enableSchedule(t, gwBad, badGroups); errno == errnoSuccess {
		t.Error("scheduler/enable: unknown workMode must be rejected")
	}
	if n := len(gwBad.cmds()); n != 0 {
		t.Errorf("submissions after bad workMode: %d", n)
	}
}

// TestWebTokenRandomWhenUnset (audit S7): without a configured web token
// the server makes a random one at start — never the predictable
// "sim-token-<station>" — and keeps it for its lifetime.
func TestWebTokenRandomWhenUnset(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mk := func() *Server {
		srv, err := New(Config{APIKey: testAPIKey, Plant: Plant{StationID: testStation, CapacityKW: testCapacity}}, st, nil, nil, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	a, b := mk(), mk()
	if tok := a.webToken(); strings.Contains(tok, testStation) || len(tok) < 32 || tok != a.webToken() {
		t.Fatalf("web token %q: predictable, short, or unstable", tok)
	}
	if a.webToken() == b.webToken() {
		t.Fatal("two servers share a web token")
	}
}
