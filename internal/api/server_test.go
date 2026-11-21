package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/device"
	"foxesslocal/internal/forward"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// testBase is the fixed clock anchor (C-004: no wall-clock in tests).
var testBase = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// testConnID is the active inverter flow every harness starts with.
const testConnID = "conn-1"

// Shared test values (single source per value; goconst).
const (
	httpGet           = http.MethodGet
	httpPost          = http.MethodPost
	httpPut           = http.MethodPut
	httpDelete        = http.MethodDelete
	headerAuth        = "Authorization"
	headerLastEventID = "Last-Event-ID"
	queryToken        = "token"
	jsonFieldName     = "name"
	jsonFieldMode     = "mode"
	jsonFieldRole     = "role"
	testTokenName     = "ha"
	jsonFieldExportW  = "export_w"
	jsonFieldImportW  = "import_w"
	jsonFieldWorkMode = "work_mode"
	testTime1101      = "11:01"
	testTime1359      = "13:59"
	pathConfigTokens  = "/api/v1/config/tokens" //nolint:gosec // G101: a URL path, not a credential.
	pathForwardMode   = "/api/v1/forward-mode"
	pathMaintenance   = "/api/v1/maintenance"
)

// Direction texts for seedFrame (the store's Dir values).
const (
	dirS2C = "s2c"
	dirC2S = "c2s"
)

// vclock is the injected virtual clock.
type vclock struct{ now time.Time }

func (c *vclock) get() time.Time          { return c.now }
func (c *vclock) advance(d time.Duration) { c.now = c.now.Add(d) }

// ---------- the scripted inverter ----------

// fakeInverter is the scripted inverter behind the real gateway: it
// records every injected s2c frame and, with auto-ack on, feeds the
// matching inverter-originated echoes back (fn03 read responses from
// cur, fn06/fn10 write ACKs) through one FIFO worker — the same
// correlation order the gateway expects from the live device.
type fakeInverter struct {
	gw  *gateway.Gateway
	cur map[int]int // the device's current register values

	mu      sync.Mutex
	frames  []frame.Frame
	autoAck bool
	ackCh   chan []byte
	// corruptFn06 makes the next fn06 echo report a different value — a
	// device read-back mismatch.
	corruptFn06 bool
}

// defaultRegisters is the device's current state: the live capture's
// 48000 r0 (scheduler master) set, everything else zero (a zero register
// is the default; the fake's map lookup yields 0 for absent keys).
func defaultRegisters() map[int]int {
	return map[int]int{regWorkModeAddr: 0}
}

// InjectS2C records one injected s2c frame (split into frames) and, with
// auto-ack on, queues its echo (the FIFO worker feeds it in order).
func (f *fakeInverter) InjectS2C(_ string, raw []byte) error {
	frs, _ := frame.Split(raw, 0)
	for _, fr := range frs {
		f.mu.Lock()
		f.frames = append(f.frames, fr)
		ack := f.autoAck
		f.mu.Unlock()
		if ack {
			if pdu := f.echo(fr); pdu != nil {
				f.ackCh <- pdu
			}
		}
	}
	return nil
}

// u16BE encodes v big-endian.
func u16BE(v int) []byte {
	//nolint:gosec // G115: bounded protocol integers.
	return []byte{byte(v >> 8), byte(v & 0xFF)}
}

// echo builds the inverter's response PDU for one injected request, or
// nil when the frame is not a Modbus request.
func (f *fakeInverter) echo(fr frame.Frame) []byte {
	mb, err := frame.ParseModbus(fr.Payload)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	cur, corrupt := f.cur, f.corruptFn06
	if corrupt {
		f.corruptFn06 = false
	}
	f.mu.Unlock()
	switch mb.Fn {
	case 0x03, 0x04:
		p := make([]byte, 3+2*mb.Count)

		p[0], p[1] = 0x01, mb.Fn
		//nolint:gosec // G115: byte count is 2×registers, bounded.
		p[2] = byte(2 * mb.Count)
		for i := range mb.Count {
			copy(p[3+2*i:], u16BE(cur[mb.Addr+i]))
		}
		return p
	case 0x06:
		val := mb.Value
		if corrupt {
			val ^= 1
		}
		return append([]byte{0x01, 0x06}, append(u16BE(mb.Addr), u16BE(val)...)...)
	case 0x10:
		return append([]byte{0x01, 0x10}, append(u16BE(mb.Addr), u16BE(mb.Count)...)...)
	}
	return nil
}

// worker feeds echoes into the gateway in FIFO order (the inverter
// answers sequentially).
func (f *fakeInverter) worker() {
	for pdu := range f.ackCh {
		id := f.inFlightID()
		if id == "" {
			continue
		}
		_ = f.gw.Ack(id, frame.Frame{Device: invLead, Fn: 0x10, Payload: pdu})
	}
}

// invLead is the inverter-originated device lead (request dev0 0x11|0x80).
var invLead = [4]byte{0x91, 0, 0, 0}

func (f *fakeInverter) inFlightID() string {
	for _, p := range f.gw.Pending() {
		if p.Status == gateway.StatusInFlight {
			return p.ID
		}
	}
	return ""
}

func (f *fakeInverter) injected() []frame.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]frame.Frame, len(f.frames))
	copy(out, f.frames)
	return out
}

// setAutoAck toggles the scripted ACKs (off = hold the command
// in flight; on = ack the next command as it lands).
func (f *fakeInverter) setAutoAck(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoAck = on
}

// corruptNextFn06 arms the read-back mismatch for the next fn06 write.
func (f *fakeInverter) corruptNextFn06() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corruptFn06 = true
}

// ---------- the scripted maintenance window ----------

// fakeMaintenance is a scripted MaintenanceState mimicking the
// forward.Maintenance seam (in-memory state, the same transition
// rules: Open re-arms when active, Close refuses when inactive).
type fakeMaintenance struct {
	mu      sync.Mutex
	active  bool
	opens   time.Time
	expires time.Time
	rearms  int
	base    string
}

func newFakeMaintenance() *fakeMaintenance {
	return &fakeMaintenance{base: "telemetry_only"}
}

func (f *fakeMaintenance) Open(d time.Duration, _ string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := testBase
	if d <= 0 {
		d = 30 * time.Minute
	}
	if d > 4*time.Hour {
		return time.Time{}, forward.ErrDurationTooLong
	}
	if f.active {
		f.expires = now.Add(d)
		f.rearms++
		return f.expires, nil
	}
	f.active = true
	f.opens = now
	f.expires = now.Add(d)
	f.rearms = 0
	return f.expires, nil
}

func (f *fakeMaintenance) Close(_ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return forward.ErrNotActive
	}
	f.active = false
	return nil
}

func (f *fakeMaintenance) View() forward.View {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return forward.View{Base: policy.ForwardMode(f.base)}
	}
	return forward.View{
		Active:    true,
		Opens:     f.opens,
		Expires:   f.expires,
		Rearms:    f.rearms,
		Base:      policy.ForwardMode(f.base),
		Remaining: f.expires.Sub(testBase),
	}
}

func (f *fakeMaintenance) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// ---------- the harness ----------

type harness struct {
	st  *store.Store
	gw  *gateway.Gateway
	inv *fakeInverter
	mnt *fakeMaintenance
	tok *TokenStore
	fwd *forward.State
	bus *Bus
	clk *vclock
	srv *Server // the constructed server (h.h is its handler)
	h   http.Handler

	tokens map[Role]string // role → plaintext
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, false)
}

// newGatedHarness is a harness whose gateway carries the store-backed
// C-008 write gate (as production wires it).
func newGatedHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, true)
}

func newHarnessWith(t *testing.T, gated bool) *harness {
	t.Helper()
	clk := &vclock{now: testBase}
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"), "dev")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	inv := &fakeInverter{cur: defaultRegisters(), autoAck: true, ackCh: make(chan []byte, 256)}
	var opts []gateway.Option
	if gated {
		opts = append(opts, gateway.WithAuthorizer(device.GatewayAuthorizer(st)))
	}
	gw := gateway.New(inv, clk.get, opts...)
	inv.gw = gw
	go inv.worker()
	t.Cleanup(func() { close(inv.ackCh) })
	gw.SetFlow(testConnID)

	tok, err := NewTokenStore("", clk.get)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	h := &harness{
		st: st, gw: gw, inv: inv, tok: tok, clk: clk,
		mnt:    newFakeMaintenance(),
		fwd:    forward.New(st, clk.get, policy.ModeTelemetryOnly),
		bus:    NewBus(),
		tokens: map[Role]string{},
	}
	for _, role := range []Role{RoleReadOnly, RoleStandard, RoleExpert, RoleInstaller} {
		_, plaintext, err := tok.Create("tok-"+string(role), role)
		if err != nil {
			t.Fatalf("create %s token: %v", role, err)
		}
		h.tokens[role] = plaintext
	}
	// New's dev defaults carry PublicReadOnly: true (the config default,
	// spec §5.5) — tests that exercise the 401 auth path flip the field
	// off on h.srv.
	h.srv = New(st, gw, h.mnt, tok, h.fwd, h.bus, clk.get)
	h.h = h.srv
	return h
}

// do issues one request. role "" = unauthenticated; any other role uses
// that role's token as the Bearer. body is JSON-marshaled when non-nil.
func (h *harness) do(t *testing.T, method, path string, role Role, body any) *httptest.ResponseRecorder {
	t.Helper()
	token := ""
	if role != "" {
		token = h.tokens[role]
	}
	return h.doAuth(t, method, path, token, body)
}

// doAuth issues one request with a raw bearer token ("" = none).
func (h *harness) doAuth(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "http://api.test"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(headerAuth, "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	return rr
}

// problemBody is the RFC 7807 error body (the spec's Problem
// schema) as decoded by the tests.
type problemBody struct {
	Type         string `json:"type"`
	Title        string `json:"title"`
	Status       int    `json:"status"`
	Detail       string `json:"detail"`
	Code         string `json:"code"`
	RequiredRole string `json:"required_role"`
	TokenRole    string `json:"token_role"`
	Errors       []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
	Applied    string     `json:"applied"`
	Setting    string     `json:"setting"`
	Remaining  *int       `json:"remaining"`
	NextFreeAt *time.Time `json:"next_free_at"`
}

// wantStatus asserts the response's status code.
func wantStatus(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status: got %d, want %d (body: %s)", rr.Code, status, rr.Body.String())
	}
}

// decodeJSON decodes the response body into v.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func checkProblem(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) problemBody {
	t.Helper()
	wantStatus(t, rr, status)
	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body: %v (body: %s)", err, rr.Body.String())
	}
	if p.Code != code {
		t.Fatalf("code: got %q, want %q (body: %s)", p.Code, code, rr.Body.String())
	}
	if p.Type == "" || p.Title == "" || p.Status != status {
		t.Fatalf("problem fields: %+v", p)
	}
	return p
}

// ---------- store seeding (the read side) ----------

// telePayload builds a 150 B tele payload (profile §4 offsets).
func telePayload(sol, grid, haus, acport, batt int32, soc, chargeState byte) []byte {
	p := make([]byte, 150)
	put32 := func(off int, v int32) {
		//nolint:gosec // G115: protocol s32 fields.
		binary.BigEndian.PutUint32(p[off:off+4], uint32(v))
	}
	put32(35, sol)
	put32(39, grid)
	put32(43, haus)
	put32(59, acport)
	put32(67, batt)
	//nolint:gosec // G115: haus fits u16 in the test fixtures.
	binary.BigEndian.PutUint16(p[65:67], uint16(haus))
	p[72] = chargeState
	p[79] = soc
	return p
}

// snap61Payload builds a 626 B snap61 payload (profile §4 TLV layout).
func snap61Payload(socPct, energyUnits uint16, volts []uint16) []byte {
	const (
		tlvBase   = 40
		modBase   = 412
		tlvStride = 12
		socOff    = 6
	)
	p := make([]byte, 626)
	putTLV := func(base, idx int, tag uint16, off int, v uint16) {
		e := base + tlvStride*idx

		binary.BigEndian.PutUint16(p[e:e+2], tag)

		binary.LittleEndian.PutUint16(p[e+2+off:e+2+off+2], v)
	}
	putTLV(tlvBase, 16, 0x1905, socOff, socPct)
	putTLV(tlvBase, 18, 0x1907, 0, energyUnits)
	for i, v := range volts {
		putTLV(modBase, i, 0x0c05+uint16(i), socOff, v)
	}
	return p
}

// fn06Pdu builds an fn06 single-register write PDU (unit 01).
func fn06Pdu(addr, val int) []byte {
	return []byte{0x01, 0x06, u16BE(addr)[0], u16BE(addr)[1], u16BE(val)[0], u16BE(val)[1]}
}

// fn10Pdu builds an fn10 write-multiple PDU (unit 01) over vals.
func fn10Pdu(addr int, vals []int) []byte {
	//nolint:gosec // G115: the byte count is 2×registers, bounded by the PDU.
	p := []byte{0x01, 0x10, u16BE(addr)[0], u16BE(addr)[1], u16BE(len(vals))[0], u16BE(len(vals))[1], byte(2 * len(vals))}
	for _, v := range vals {
		p = append(p, u16BE(v)...)
	}
	return p
}

// slotRegisters packs eight 48010 groups (en, start, end, wm, maxMin,
// fdsoc, fdpwr per group; +7 reserved, +8/+9 zero).
func slotRegisters(groups [8][7]int) []int {
	out := make([]int, 0, 80)
	for g := range groups {
		for _, v := range groups[g] {
			out = append(out, v)
		}
		out = append(out, 0, 0, 0)
	}
	return out
}

// seedFrame records one frame row and flushes (the rows must be visible
// to the queries under test). dir is "c2s" or "s2c" (the store's
// direction text).
func (h *harness) seedFrame(ts time.Time, dir string, class frame.Kind, decision store.Decision, payload []byte, decoded json.RawMessage) {
	h.st.RecordFrame(store.FrameRecord{
		ConnID: testConnID, Dir: dirOf(dir), TS: ts, Class: class,
		Off: 0, Dev: [4]byte{0xaa, 0x6b, 0x01, 0x19}, Fn: 0x10, Plen: len(payload),
		Payload: payload, Decision: decision, Origin: "", Decoded: decoded,
	})
	h.st.Flush()
}

// dirOf maps the store's direction text to the capture type.
func dirOf(dir string) capture.Dir {
	if dir == dirS2C {
		return capture.DirS2C
	}
	return capture.DirC2S
}

func (h *harness) seedTele(ts time.Time, sol, grid, haus, acport, batt int32, soc, chargeState byte) {
	h.seedFrame(ts, dirC2S, classTele, decisionForwarded,
		telePayload(sol, grid, haus, acport, batt, soc, chargeState), nil)
}

func (h *harness) seedSnap61(ts time.Time, socPct, energyUnits uint16, volts []uint16) {
	h.seedFrame(ts, dirC2S, classSnap61, decisionForwarded,
		snap61Payload(socPct, energyUnits, volts), nil)
}

// seedMbWrite records an fn10/fn06 write PDU row (the cloud's
// write — always s2c).
func (h *harness) seedMbWrite(ts time.Time, pdu []byte) {
	h.seedFrame(ts, dirS2C, classMbWrite, decisionForwarded, pdu, nil)
}

// TestInternalErrorIsGeneric (audit S11): an internal error's text is
// logged, not returned to the client.
func TestInternalErrorIsGeneric(t *testing.T) {
	rr := httptest.NewRecorder()
	(&Server{}).internal(rr, errors.New("open /var/lib/foxess-local/foxess.db: disk I/O error"))
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "/var/lib") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}
