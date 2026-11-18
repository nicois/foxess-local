package sim

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

// Test fixtures (the profile §4/§5 layout, mon150 offsets).
const (
	testAPIKey   = "test-api-key"
	testStation  = "sim-plant-1"
	testCapacity = 10
	testSN       = "INVSN12345678"
	batterySN1   = "BATSN0000001"
	batterySN2   = "BATSN0000002"

	// JSON request keys (the integration's POST bodies; goconst).
	keyCurrentPage = "currentPage"
	keyPageSize    = "pageSize"
	keyDeviceSN    = "deviceSN"
	keyGroups      = "groups"
	keyMinSocGrid  = "minSocOnGrid"

	snap01Len = 141
	snap60Len = 280
	snap61Len = 626

	// 150 B tele field offsets (profile §4).
	teleLen   = 150
	teleSol   = 35
	teleGrid  = 39
	teleHaus  = 43
	teleAc    = 59
	teleHausD = 65
	teleBatt  = 67
	teleChg   = 72
	teleSoc   = 79

	// 48010 slot block (mon150 printSlotTable).
	groupRegs  = 10
	regBytes   = 2
	schedAddr  = 48010
	schedCnt   = 80
	slotDataB  = schedCnt * regBytes
	fn10HdrLen = 7
)

// ---------- fake gateway ----------

// fakeGW records the submitted commands and returns a scripted Wait result.
type fakeGW struct {
	mu        sync.Mutex
	submitted []gateway.Command
	submitErr error
	status    gateway.Status
	detail    string
}

func (f *fakeGW) Submit(_ context.Context, c gateway.Command) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return "", f.submitErr
	}
	f.submitted = append(f.submitted, c)
	return "fake-1", nil
}

func (f *fakeGW) Wait(_ context.Context, id string) (gateway.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status
	if st == "" {
		st = gateway.StatusAcked
	}
	var kind gateway.CommandKind
	if n := len(f.submitted); n > 0 {
		kind = f.submitted[n-1].Kind
	}
	return gateway.Result{ID: id, Kind: kind, Status: st, Detail: f.detail}, nil
}

func (f *fakeGW) cmds() []gateway.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gateway.Command(nil), f.submitted...)
}

// ---------- test server ----------

// testServer is a sim.Server wired to a fake gateway and an event channel.
type testServer struct {
	t      *testing.T
	st     *store.Store
	gw     *fakeGW
	events chan string // the alerts' details
	srv    *Server
	rec    *httptest.Server
}

// newTestServer builds a server with the standard test config (the
// integration-visible surface: api key, plant, capacity).
func newTestServer(t *testing.T, st *store.Store, gw *fakeGW) *testServer {
	t.Helper()
	events := make(chan string, 32)
	emit := func(detail string) { events <- detail }
	srv, err := New(Config{
		APIKey:   testAPIKey,
		WebToken: wantWebToken,
		Plant:    Plant{StationID: testStation, CapacityKW: testCapacity},
	}, st, gw, emit, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := &testServer{t: t, st: st, gw: gw, events: events, srv: srv}
	ts.rec = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.rec.Close)
	return ts
}

// do performs one authenticated request (token header, the integration's
// /op/v0 convention) and returns the recorder.
func (ts *testServer) do(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", testAPIKey)
	rr := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(rr, req)
	return rr
}

// decodeEnvelope parses the cloud envelope from a response body.
func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope: %v: %s", err, body)
	}
	return env
}

// ---------- store seeding ----------

// openTestStore opens a temp store with cleanup.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "dev")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// telePayload builds a 150 B telemetry payload (profile §4 offsets).
func telePayload(sol, grid, haus, ac, batt int32, soc, charge byte) []byte {
	p := make([]byte, teleLen)
	putS32 := func(off int, v int32) {
		//nolint:gosec // G115: signed 32-bit protocol field.
		binary.BigEndian.PutUint32(p[off:off+4], uint32(v))
	}
	putS32(teleSol, sol)
	putS32(teleGrid, grid)
	putS32(teleHaus, haus)
	putS32(teleAc, ac)
	//nolint:gosec // G115: fixture value, bounded to 16 bits.
	binary.BigEndian.PutUint16(p[teleHausD:teleHausD+2], uint16(haus))
	putS32(teleBatt, batt)
	p[teleChg] = charge
	p[teleSoc] = soc
	return p
}

// snap01Payload builds a 141 B device-identity payload (SN, four firmware
// strings, model — mon150 devinfo).
func snap01Payload() []byte {
	p := make([]byte, snap01Len)
	copy(p[0:15], testSN)
	copy(p[33:37], "1.73")
	copy(p[39:43], "1.03")
	copy(p[45:49], "1.71")
	copy(p[51:55], "0.00")
	copy(p[93:100], "KH10")
	return p
}

// snap60Payload builds a 280 B battery-module-list payload (mon150: ten
// 15-char slots at [50+21k:65+21k]).
func snap60Payload() []byte {
	p := make([]byte, snap60Len)
	copy(p[0:15], testSN)
	for k, sn := range []string{batterySN1, batterySN2} {
		copy(p[50+21*k:65+21*k], sn)
	}
	return p
}

// snap61Payload builds a 626 B battery-status payload (SoC, stored energy,
// two module-table bus voltages at the profile §4 TLV layout).
func snap61Payload(soc, energyUnits uint16) []byte {
	p := make([]byte, snap61Len)
	copy(p, testSN)
	putTLV := func(base, idx, tag, off int, v uint16) {
		e := base + 12*idx
		//nolint:gosec // G115: protocol tag, bounded to 16 bits.
		binary.BigEndian.PutUint16(p[e:e+2], uint16(tag))
		binary.LittleEndian.PutUint16(p[e+2+off:e+4+off], v)
	}
	putTLV(40, 16, 0x1905, 6, soc)
	putTLV(40, 18, 0x1907, 0, energyUnits)
	putTLV(412, 0, 0x0c05, 6, 52000)
	putTLV(412, 1, 0x0c06, 6, 53000)
	return p
}

// testSlot is one 48010 group (mon150 printSlotTable fields).
type testSlot struct {
	en, start, end, wm, maxMin, fdSoc, fdPwr int
}

// slotData packs eight groups into the 48010 x80 register block.
func slotData(groups [8]testSlot) []byte {
	p := make([]byte, slotDataB)
	for k, g := range groups {
		vals := [7]int{g.en, g.start, g.end, g.wm, g.maxMin, g.fdSoc, g.fdPwr}
		for i, v := range vals {
			//nolint:gosec // G115: 16-bit register value.
			binary.BigEndian.PutUint16(p[(groupRegs*k+i)*regBytes:], uint16(v))
		}
	}
	return p
}

// mbWrite48010 builds the fn10 write-multiple PDU for a full 48010 push.
func mbWrite48010(groups [8]testSlot) []byte {
	p := make([]byte, fn10HdrLen+slotDataB)
	p[0] = 1
	p[1] = 0x10
	binary.BigEndian.PutUint16(p[2:4], schedAddr)
	binary.BigEndian.PutUint16(p[4:6], schedCnt)
	p[6] = byte(slotDataB)
	copy(p[fn10HdrLen:], slotData(groups))
	return p
}

// seedStore records the frames the endpoint tests assert on: a 150 B tele,
// the identity snapshots (snap01/snap60), the snap61 battery snapshot, and
// the 48010 schedule push — then flushes.
func seedStore(t *testing.T, st *store.Store, base time.Time) {
	t.Helper()
	rec := func(seq int, class frame.Kind, dev0 byte, payload []byte, ts time.Time) {
		st.RecordFrame(store.FrameRecord{
			ConnID: "conn-1", Seq: seq, Dir: capture.DirC2S, TS: ts,
			Class: class, Dev: [4]byte{dev0}, Fn: 0x10,
			Plen: len(payload), Payload: payload, Decision: "forwarded",
		})
	}
	rec(1, "tele", 0x05, telePayload(2500, -800, 1200, 500, 1500, 42, 4), base.Add(10*time.Second))
	rec(2, "snap01", 0x01, snap01Payload(), base.Add(20*time.Second))
	rec(3, "snap60", 0x60, snap60Payload(), base.Add(25*time.Second))
	rec(4, "snap61", 0x61, snap61Payload(42, 2070), base.Add(30*time.Second))
	sched := [8]testSlot{}
	sched[0] = testSlot{en: 1, start: 0, end: 23<<8 | 59, wm: 1, maxMin: 80<<8 | 10, fdSoc: 30, fdPwr: 5000}
	sched[2] = testSlot{en: 1, start: 18 << 8, end: 20 << 8, wm: 7, maxMin: 100<<8 | 17, fdSoc: 20, fdPwr: 100}
	// the schedule push travels cloud → inverter
	st.RecordFrame(store.FrameRecord{
		ConnID: "conn-1", Seq: 5, Dir: capture.DirS2C, TS: base.Add(40 * time.Second),
		Class: "mbwrite", Dev: [4]byte{0x11}, Fn: 0x10,
		Plen: len(mbWrite48010(sched)), Payload: mbWrite48010(sched), Decision: "forwarded",
	})
	st.Flush()
}

// ---------- minimal RFC 6455 client ----------

// wsTestClient is the tests' WebSocket client: masked frames to the server,
// unmasked frames from it.
type wsTestClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialWS opens a WebSocket to addr (host:port) with the given query token.
func dialWS(t *testing.T, addr, token string) *wsTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("ws key: %v", err)
	}
	req := fmt.Sprintf(
		"GET /dew/v0/wsmaitian?plantId=%s&token=%s&platform=web&lang=en HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\n"+
			"Sec-WebSocket-Version: 13\r\n\r\n",
		testStation, url.QueryEscape(token), addr,
		base64.StdEncoding.EncodeToString(key))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("ws request: %v", err)
	}
	br := bufio.NewReader(conn)
	wsReadHandshake(t, br)
	return &wsTestClient{conn: conn, br: br}
}

// wsReadHandshake consumes the 101 response + headers from br.
func wsReadHandshake(t *testing.T, br *bufio.Reader) {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ws status: %v", err)
	}
	if !strings.Contains(line, "101") {
		t.Fatalf("ws upgrade: got %q", line)
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ws headers: %v", err)
		}
		if strings.TrimRight(h, "\r\n") == "" {
			break
		}
	}
}

// sendText sends one masked text frame.
func (c *wsTestClient) sendText(t *testing.T, s string) {
	t.Helper()
	payload := []byte(s)
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		t.Fatalf("ws mask: %v", err)
	}
	out := make([]byte, 0, 6+len(payload))
	//nolint:gosec // G115: test payloads are far below 127 bytes.
	out = append(out, 0x81, 0x80|byte(len(payload)))
	out = append(out, mask...)
	for i, b := range payload {
		out = append(out, b^mask[i&3])
	}
	if _, err := c.conn.Write(out); err != nil {
		t.Fatalf("ws send: %v", err)
	}
}

// readFrame reads one server frame (unmasked per RFC 6455).
func (c *wsTestClient) readFrame(t *testing.T) (byte, []byte) {
	t.Helper()
	h := make([]byte, 2)
	if _, err := io.ReadFull(c.br, h); err != nil {
		t.Fatalf("ws frame head: %v", err)
	}
	op := h[0] & 0x0F
	if h[0]&0x80 == 0 {
		t.Fatalf("ws frame: FIN must be set")
	}
	if h[1]&0x80 != 0 {
		t.Fatalf("ws frame: server frames must be unmasked")
	}
	n := int(h[1] & wsLenMask)
	switch n {
	case wsLen16:
		e := make([]byte, wsHdrSize)
		if _, err := io.ReadFull(c.br, e); err != nil {
			t.Fatalf("ws frame len16: %v", err)
		}

		n = int(binary.BigEndian.Uint16(e))
	case wsLen64:
		e := make([]byte, wsLen64Size)
		if _, err := io.ReadFull(c.br, e); err != nil {
			t.Fatalf("ws frame len64: %v", err)
		}
		//nolint:gosec // G115: test frames are bounded to 64 KiB.
		n = int(binary.BigEndian.Uint64(e))
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatalf("ws frame payload: %v", err)
	}
	return op, payload
}

// readText blocks until one text frame arrives and returns its payload.
func (c *wsTestClient) readText(t *testing.T) string {
	t.Helper()
	for {
		op, payload := c.readFrame(t)
		switch op {
		case 0x1: // text
			return string(payload)
		case 0xA: // pong: the server answers its own pings only; skip
		case 0x9: // ping: answer it
			c.sendPong(t, payload)
		case 0x8: // close
			t.Fatalf("ws closed by server")
		default:
			t.Fatalf("ws opcode: 0x%x", op)
		}
	}
}

// sendPong answers a ping with the same payload (masked).
func (c *wsTestClient) sendPong(t *testing.T, payload []byte) {
	t.Helper()
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		t.Fatalf("ws mask: %v", err)
	}
	out := make([]byte, 0, 6+len(payload))
	//nolint:gosec // G115: ping payloads are far below 127 bytes.
	out = append(out, 0x8A, 0x80|byte(len(payload)))
	out = append(out, mask...)
	for i, b := range payload {
		out = append(out, b^mask[i&3])
	}
	if _, err := c.conn.Write(out); err != nil {
		t.Fatalf("ws pong: %v", err)
	}
}
