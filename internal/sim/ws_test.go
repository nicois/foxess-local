package sim

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/chanpipe"

	"foxesslocal/internal/store"
)

// wsMsg is the wsmaitian message as the integration parses it
// (realtime_ws.py: errno, result.timeDiff, result.node).
type wsMsg struct {
	Errno  int `json:"errno"`
	Result struct {
		TimeDiff int `json:"timeDiff"`
		Node     *struct {
			Bat struct {
				Soc    int    `json:"soc"`
				Charge string `json:"charge"`
				Power  struct {
					Value string `json:"value"`
					Unit  string `json:"unit"`
				} `json:"power"`
				BatteryID string `json:"batteryId"`
				Multiple  []struct {
					BatSn string `json:"batSn"`
					Soc   int    `json:"soc"`
				} `json:"multipleBatterySoc"`
			} `json:"bat"`
			Solar struct {
				Power struct {
					Value string `json:"value"`
				} `json:"power"`
			} `json:"solar"`
			Load struct {
				Power struct {
					Value string `json:"value"`
				} `json:"power"`
			} `json:"load"`
			Grid struct {
				Power struct {
					Value string `json:"value"`
				} `json:"power"`
				GridStatus int `json:"gridStatus"`
			} `json:"grid"`
		} `json:"node"`
	} `json:"result"`
}

// parseWS decodes one wire message.
func parseWS(t *testing.T, raw string) wsMsg {
	t.Helper()
	var m wsMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("ws message: %v: %s", err, raw)
	}
	return m
}

// wsListener serves the sim handler on a loopback TCP listener.
func wsListener(t *testing.T, srv *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	return l.Addr().String()
}

// checkFeedNode pins the real-time node values for the seeded fixture:
// the bat node's batteryId@batSn compound id (the integration's
// discovery path), the tele powers in watts, and the grid status from
// the panel balance.
func checkFeedNode(t *testing.T, m wsMsg) {
	t.Helper()
	if m.Errno != errnoSuccess {
		t.Fatalf("errno: %d", m.Errno)
	}
	node := m.Result.Node
	if node == nil {
		t.Fatal("no node")
	}
	bat := node.Bat
	if bat.BatteryID != "sim-battery-"+testSN {
		t.Errorf("batteryId: %q", bat.BatteryID)
	}
	if len(bat.Multiple) == 0 || bat.Multiple[0].BatSn != batterySN1 {
		t.Errorf("batSn: %+v", bat.Multiple)
	}
	if bat.Soc != 42 {
		t.Errorf("soc: %d (want 42)", bat.Soc)
	}
	if bat.Charge != "1" {
		t.Errorf("charge: %q (want 1, Batt +1500)", bat.Charge)
	}
	if bat.Power.Value != "1500" || bat.Power.Unit != "W" {
		t.Errorf("bat power: %+v", bat.Power)
	}
	if node.Solar.Power.Value != "2500" {
		t.Errorf("solar: %q", node.Solar.Power.Value)
	}
	if node.Load.Power.Value != "1200" {
		t.Errorf("load: %q", node.Load.Power.Value)
	}
	if node.Grid.Power.Value != "800" {
		t.Errorf("grid value: %q (want 800, |Grid|)", node.Grid.Power.Value)
	}
	if node.Grid.GridStatus != 2 {
		t.Errorf("gridStatus: %d (want 2, exporting)", node.Grid.GridStatus)
	}
}

// TestWSGetdataCompoundID pins the real-time node: the bat node carries
// the batteryId@batSn compound id (the integration's discovery path), the
// tele powers in watts, and the grid status from the panel balance.
func TestWSGetdataCompoundID(t *testing.T) {
	st := openTestStore(t)
	// now-relative base so the feed reads fresh (timeDiff < 30).
	seedStore(t, st, time.Now().Add(-10*time.Second))
	events := make(chan string, 16)
	srv, err := New(Config{
		APIKey:   testAPIKey,
		WebToken: wantWebToken,
		Plant:    Plant{StationID: testStation, CapacityKW: testCapacity},
	}, st, &fakeGW{}, func(detail string) { events <- detail }, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := wsListener(t, srv)

	c := dialWS(t, addr, wantWebToken)
	c.sendText(t, "getdata")
	m := parseWS(t, c.readText(t))

	if m.Result.TimeDiff < 0 || m.Result.TimeDiff > 60 {
		t.Errorf("timeDiff: %d (want fresh, < 60)", m.Result.TimeDiff)
	}
	checkFeedNode(t, m)
}

// TestWSCadenceVirtual pins the 5 s push cadence under virtual time:
// consecutive ticker pushes are exactly 5 s apart (C-004: no wall-clock
// sleeps).
func TestWSCadenceVirtual(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, time.Now().Add(-10*time.Second))
	events := make(chan string, 16)
	srv, err := New(Config{
		APIKey:   testAPIKey,
		WebToken: wantWebToken,
		Plant:    Plant{StationID: testStation, CapacityKW: testCapacity},
	}, st, &fakeGW{}, func(detail string) { events <- detail }, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The SQLite I/O must stay out of the synctest bubble (the framesvc
	// harness pattern): seed the identity cache and capture the state
	// once, then inject a pure state closure for the push loop.
	if id := srv.deviceID(); id.SN == "" {
		t.Fatal("identity not seeded")
	}
	snap, err := srv.state()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	srv.state = func() (store.State, error) { return snap, nil }

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go srv.Start(ctx) // joins the bubble (the push loop)

		// The WS leg is a chanpipe created inside the bubble (synctest:
		// the clock advances only while the group is durably blocked —
		// bubble channels qualify, kernel sockets do not).
		serverEnd, clientEnd := chanpipe.New()
		t.Cleanup(func() {
			_ = clientEnd.Close()
			_ = serverEnd.Close()
		})
		go srv.Handler().ServeHTTP(&wsHijackWriter{conn: serverEnd}, wsUpgradeReq())

		br := bufio.NewReader(clientEnd)
		wsReadHandshake(t, br) // the 101 arrives on the pipe
		c := &wsTestClient{conn: clientEnd, br: br}

		ma := parseWS(t, c.readText(t)) // first tick that finds the client
		mb := parseWS(t, c.readText(t)) // next tick
		if ma.Errno != errnoSuccess || mb.Errno != errnoSuccess {
			t.Fatalf("errno: %d / %d", ma.Errno, mb.Errno)
		}
		if ma.Result.Node == nil || mb.Result.Node == nil {
			t.Fatal("ticker pushes must carry the node")
		}
		if delta := mb.Result.TimeDiff - ma.Result.TimeDiff; delta != int(wsPushPeriod/time.Second) {
			t.Errorf("cadence: %d -> %d (delta %d, want %d)",
				ma.Result.TimeDiff, mb.Result.TimeDiff, delta, int(wsPushPeriod/time.Second))
		}
	})
}

// TestWSPushTickStaleClient pins the stale-client rule (reference-simulator
// parity): on a tick the newest client gets the real message and older
// clients the no-data keepalive; getdata always answers with real data.
func TestWSPushTickStaleClient(t *testing.T) {
	st := openTestStore(t)
	seedStore(t, st, testBase)
	events := make(chan string, 16)
	srv, err := New(Config{
		APIKey:   testAPIKey,
		WebToken: wantWebToken,
		Plant:    Plant{StationID: testStation, CapacityKW: testCapacity},
	}, st, &fakeGW{}, func(detail string) { events <- detail }, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := wsListener(t, srv)

	c1 := dialWS(t, addr, wantWebToken)
	waitClients(t, srv, 1) // the server registers a client after the handshake
	c2 := dialWS(t, addr, wantWebToken)
	waitClients(t, srv, 2)
	srv.pushTick()

	stale := parseWS(t, c1.readText(t))
	fresh := parseWS(t, c2.readText(t))
	if stale.Result.TimeDiff != staleTimeDiff || stale.Result.Node != nil {
		t.Errorf("older client: timeDiff %d node %v (want the keepalive)",
			stale.Result.TimeDiff, stale.Result.Node)
	}
	if fresh.Result.Node == nil {
		t.Error("newest client: must get the real message")
	}

	// getdata on the older client answers with real data.
	c1.sendText(t, "getdata")
	got := parseWS(t, c1.readText(t))
	if got.Result.Node == nil {
		t.Error("getdata must answer with the real message")
	}
}

// wsHijackWriter is the minimal http.ResponseWriter the WS handler needs
// over an in-memory leg: the upgrade response goes through the hijacked
// writer; the handler's envelope writes are unreachable (auth succeeds).
type wsHijackWriter struct {
	conn net.Conn
}

func (w *wsHijackWriter) Header() http.Header {
	return http.Header{}
}

func (w *wsHijackWriter) WriteHeader(int) {}

func (w *wsHijackWriter) Write([]byte) (int, error) {
	return 0, nil
}

func (w *wsHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

// wsUpgradeReq builds the upgrade request the handler sees (the token in
// the query, as the integration sends it). The request is a struct — the
// client leg therefore never writes the raw request line.
func wsUpgradeReq() *http.Request {
	q := url.Values{}
	q.Set("plantId", testStation)
	q.Set("token", wantWebToken)
	q.Set("platform", "web")
	q.Set("lang", "en")
	h := http.Header{}
	h.Set("Upgrade", "websocket")
	h.Set("Connection", "Upgrade")
	h.Set("Sec-WebSocket-Version", "13")
	h.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") // any non-empty key
	return &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/dew/v0/wsmaitian", RawQuery: q.Encode()},
		Header: h,
		Body:   http.NoBody,
	}
}

// TestWSAuth pins the upgrade auth: the web token in the query is checked
// before the upgrade (wrong token → 401, no 101).
func TestWSAuth(t *testing.T) {
	st := openTestStore(t)
	srv, err := New(Config{
		APIKey:   testAPIKey,
		WebToken: wantWebToken,
		Plant:    Plant{StationID: testStation, CapacityKW: testCapacity},
	}, st, &fakeGW{}, func(string) {}, time.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := wsListener(t, srv)

	resp, err := http.Get("http://" + addr + "/dew/v0/wsmaitian?plantId=" + testStation + "&token=wrong")
	if err != nil {
		t.Fatalf("ws auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("ws wrong token: got %d, want 401", resp.StatusCode)
	}
}

// waitClients waits until the server has registered n WS clients: it
// registers one after answering the upgrade, so a dial can return first.
func waitClients(t *testing.T, srv *Server, n int) {
	t.Helper()
	for range 5000 {
		srv.wsMu.Lock()
		got := len(srv.clients)
		srv.wsMu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the server never registered %d WS clients", n)
}
