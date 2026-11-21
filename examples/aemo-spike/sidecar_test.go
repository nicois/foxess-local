package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseDispatchCSV: the DISPATCH PRICE rows of a real report
// (testdata, public AEMO data, trimmed to the first 16 columns): each
// region's RRP and interval end in NEM time.
func TestParseDispatchCSV(t *testing.T) {
	f, err := os.Open("testdata/dispatch-price.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	prices, err := parseDispatchCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	vic := prices[vic1]
	want := time.Date(2026, 11, 21, 0, 50, 0, 0, time.UTC)
	if len(prices) != 5 || vic.RRP != 35.94 || !vic.At.Equal(want) || prices["SA1"].RRP != -39.02 {
		t.Fatalf("prices %+v", prices)
	}
}

// fakeNEMWeb serves a listing naming file and the file as a zip of csv.
func fakeNEMWeb(t *testing.T, file string, csv []byte) (*httptest.Server, *int) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(strings.TrimSuffix(file, ".zip") + ".CSV")
	_, _ = w.Write(csv)
	_ = zw.Close()
	zips := 0
	mux := http.NewServeMux()
	mux.HandleFunc(dispatchDir, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == dispatchDir {
			_, _ = w.Write([]byte(`<A HREF="/Reports/CURRENT/DispatchIS_Reports/PUBLIC_DISPATCHIS_202611211045_0000000549774000.zip">old</A><br>` +
				`<A HREF="/Reports/CURRENT/DispatchIS_Reports/` + file + `">` + file + `</A><br>`))
			return
		}
		if r.URL.Path != dispatchDir+file {
			http.NotFound(w, r)
			return
		}
		zips++
		_, _ = w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &zips
}

// TestNEMWebLatest: the newest report in the listing is fetched once
// and its region's price returned; an unknown region is an error.
func TestNEMWebLatest(t *testing.T) {
	csv, err := os.ReadFile("testdata/dispatch-price.csv")
	if err != nil {
		t.Fatal(err)
	}
	srv, zips := fakeNEMWeb(t, "PUBLIC_DISPATCHIS_202611211050_0000000549774272.zip", csv)
	n := &NEMWeb{Base: srv.URL, Client: srv.Client()}
	for range 2 {
		p, err := n.Latest(context.Background(), vic1)
		if err != nil || p.RRP != 35.94 {
			t.Fatalf("%+v %v", p, err)
		}
	}
	if *zips != 1 {
		t.Fatalf("fetched the report %d times", *zips)
	}
	if _, err := n.Latest(context.Background(), "WA1"); err == nil {
		t.Fatal("WA1 has a price")
	}
}

const (
	vic1      = "VIC1"
	keyActive = "active"
)

var testPolicy = Policy{
	StartAbove: 300, StopBelow: 200, StartSoc: 40, CutoffSoc: 30,
	MinHold: 10 * time.Minute, Lease: 30 * time.Minute, RenewBefore: 10 * time.Minute, PriceMaxAge: 15 * time.Minute,
}

// TestDecide: start on a spike, hold while the lease has time, renew
// near its end, keep going above the stop price and for the minimum
// hold, stop below it; never start low, stop at the cut-off, and treat a
// stale price as none.
func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 21, 7, 2, 0, 0, time.UTC)
	price := func(rrp float64, age time.Duration) *Price {
		return &Price{Region: vic1, RRP: rrp, At: now.Add(-age)}
	}
	active := func(since, left time.Duration) Inputs {
		return Inputs{Now: now, Active: true, Since: now.Add(-since), Until: now.Add(left)}
	}
	with := func(in Inputs, p *Price, soc int) Inputs {
		in.Now, in.Price, in.SocPct = now, p, soc
		return in
	}
	idle := Inputs{}
	cases := []struct {
		name string
		in   Inputs
		want Action
	}{
		{"spike starts", with(idle, price(1500, time.Minute), 80), Start},
		{"normal price holds", with(idle, price(90, time.Minute), 80), Hold},
		{"spike, SoC too low to start", with(idle, price(1500, time.Minute), 35), Hold},
		{"stale spike", with(idle, price(1500, 20*time.Minute), 80), Hold},
		{"running, lease has time", with(active(20*time.Minute, 20*time.Minute), price(1500, time.Minute), 70), Hold},
		{"running, lease ending", with(active(25*time.Minute, 5*time.Minute), price(1500, time.Minute), 70), Start},
		{"running above the stop price", with(active(20*time.Minute, 20*time.Minute), price(250, time.Minute), 70), Hold},
		{"running below the stop price", with(active(20*time.Minute, 20*time.Minute), price(150, time.Minute), 70), Stop},
		{"minimum hold", with(active(5*time.Minute, 25*time.Minute), price(150, time.Minute), 70), Hold},
		{"cut-off reached", with(active(20*time.Minute, 20*time.Minute), price(1500, time.Minute), 30), Stop},
		{"running below start SoC continues", with(active(20*time.Minute, 20*time.Minute), price(1500, time.Minute), 35), Hold},
		{"SoC unknown", with(active(20*time.Minute, 20*time.Minute), price(1500, time.Minute), -1), Stop},
	}
	for _, c := range cases {
		if d := testPolicy.Decide(c.in); d.Action != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, actionName[d.Action], d.Reason, actionName[c.want])
		}
	}
	if d := testPolicy.Decide(with(idle, price(1500, time.Minute), 80)); !d.Until.Equal(now.Add(33 * time.Minute)) {
		t.Errorf("lease end %v, want rounded up to 07:35", d.Until)
	}
}

// fixedPrice is a PriceSource with one price.
type fixedPrice struct{ p Price }

func (f fixedPrice) Latest(context.Context, string) (Price, error) { return f.p, nil }

// TestTickStartsAndNotifies: on a spike the tick puts the override with
// the cut-off and power, then sends one notice; an override held by
// another token is left alone.
func TestTickStartsAndNotifies(t *testing.T) {
	var (
		mu     sync.Mutex
		calls  []string
		holder = ""
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/state":
			ov := map[string]any{keyActive: false}
			if holder != "" {
				ov = map[string]any{keyActive: true, "holder": holder, "until": time.Now().Add(time.Hour)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"online": true, "battery": map[string]any{"soc_pct": 80}, "schedule_override": ov})
		case "/api/v1/schedule/override":
			if body["fd_soc_pct"] != 30.0 || body["fd_pwr_w"] != 5000.0 || body["mode"] != "force-discharge" {
				t.Errorf("override body %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{keyActive: true, "until": body["until"]})
		case "/api/v1/notices":
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	now := time.Now()
	s := &sidecar{
		cfg: config{name: "aemo-spike", region: vic1, powerW: 5000, policy: testPolicy},
		api: &Client{Base: srv.URL, Token: "tok", Client: srv.Client()},
		src: fixedPrice{Price{Region: vic1, RRP: 2000, At: now}},
	}
	s.tick(context.Background(), now)
	mu.Lock()
	got := strings.Join(calls, ", ")
	calls, holder = nil, "someone-else"
	mu.Unlock()
	if got != "GET /api/v1/state, PUT /api/v1/schedule/override, POST /api/v1/notices" {
		t.Fatalf("calls: %s", got)
	}
	s.tick(context.Background(), now)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ", ") != "GET /api/v1/state" {
		t.Fatalf("another holder: %v", calls)
	}
}

// TestNextTick: offset past the next 5-minute boundary.
func TestNextTick(t *testing.T) {
	now := time.Date(2026, 11, 21, 7, 2, 0, 0, time.UTC)
	if d := nextTick(now, 40*time.Second); d != 3*time.Minute+40*time.Second {
		t.Fatalf("%v", d)
	}
	if d := nextTick(now.Add(-100*time.Second), 40*time.Second); d != 20*time.Second {
		t.Fatalf("%v", d)
	}
}
