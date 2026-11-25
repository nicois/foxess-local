package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

const (
	oldReport  = "PUBLIC_DISPATCHIS_202611211045_0000000549774000.zip"
	report     = "PUBLIC_DISPATCHIS_202611211050_0000000549774272.zip"
	nextReport = "PUBLIC_DISPATCHIS_202611211055_0000000549774500.zip"
)

// fakeNEMWeb is an in-memory NEMWeb (no sockets, so it runs inside a
// synctest bubble): the listing names an older report plus newest, and
// each report is served as a zip of its CSV.
type fakeNEMWeb struct {
	mu       sync.Mutex
	newest   string
	reports  map[string][]byte // name → zip
	listings int
	zips     int
}

func newFakeNEMWeb(t *testing.T) *fakeNEMWeb {
	t.Helper()
	return &fakeNEMWeb{reports: map[string][]byte{}}
}

// publish adds a report built from csv and makes it the newest.
func (f *fakeNEMWeb) publish(t *testing.T, name string, csv []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(strings.TrimSuffix(name, ".zip") + ".CSV")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(csv)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports[name], f.newest = buf.Bytes(), name
}

// client is a NEMWeb reading f.
func (f *fakeNEMWeb) client() *NEMWeb {
	return &NEMWeb{Base: "http://nemweb.test", Client: &http.Client{Transport: f}}
}

func (f *fakeNEMWeb) counts() (listings, zips int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listings, f.zips
}

func (f *fakeNEMWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := httptest.NewRecorder()
	switch name := strings.TrimPrefix(r.URL.Path, dispatchDir); {
	case r.URL.Path == dispatchDir:
		f.listings++
		_, _ = rec.WriteString(`<A HREF="/Reports/CURRENT/DispatchIS_Reports/` + oldReport + `">old</A><br>` +
			`<A HREF="/Reports/CURRENT/DispatchIS_Reports/` + f.newest + `">` + f.newest + `</A><br>`)
	case f.reports[name] != nil:
		f.zips++
		_, _ = rec.Write(f.reports[name])
	default:
		rec.WriteHeader(http.StatusNotFound)
	}
	return rec.Result(), nil
}

func dispatchCSV(t *testing.T) []byte {
	t.Helper()
	csv, err := os.ReadFile("testdata/dispatch-price.csv")
	if err != nil {
		t.Fatal(err)
	}
	return csv
}

// TestNEMWebLatest: the newest report is fetched and its region's price
// returned; an unknown region is an error, at once.
func TestNEMWebLatest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeNEMWeb(t)
		f.publish(t, report, dispatchCSV(t))
		n := f.client()
		start := time.Now()
		if p, err := n.Latest(t.Context(), vic1); err != nil || p.RRP != 35.94 {
			t.Fatalf("%+v %v", p, err)
		}
		if _, zips := f.counts(); zips != 1 {
			t.Fatalf("fetched the report %d times", zips)
		}
		n = f.client()
		if _, err := n.Latest(t.Context(), "WA1"); err == nil {
			t.Fatal("WA1 has a price")
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("took %v, want no wait", d)
		}
	})
}

// TestNEMWebLatestWaitsForNewReport: when the listing still names the
// report already read, Latest rechecks every retryPeriod and returns
// the new report's price as soon as it appears; the old price is never
// returned again.
func TestNEMWebLatestWaitsForNewReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		csv := dispatchCSV(t)
		f := newFakeNEMWeb(t)
		f.publish(t, report, csv)
		n := f.client()
		if _, err := n.Latest(t.Context(), vic1); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(12 * time.Second)
			f.publish(t, nextReport, bytes.ReplaceAll(csv, []byte("35.94"), []byte("412.5")))
		}()
		start := time.Now()
		p, err := n.Latest(t.Context(), vic1)
		if err != nil || p.RRP != 412.5 {
			t.Fatalf("%+v %v", p, err)
		}
		// one listing for the first call, then at 0, 5, 10 and 15 s
		if d := time.Since(start); d != 3*retryPeriod {
			t.Fatalf("took %v, want %v", d, 3*retryPeriod)
		}
		if listings, zips := f.counts(); listings != 5 || zips != 2 {
			t.Fatalf("%d listings, %d zips", listings, zips)
		}
	})
}

// TestNEMWebLatestNoNewReport: no new report within retryWindow is an
// error (the deadline), not the previous price.
func TestNEMWebLatestNoNewReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeNEMWeb(t)
		f.publish(t, report, dispatchCSV(t))
		n := f.client()
		if _, err := n.Latest(t.Context(), vic1); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if p, err := n.Latest(t.Context(), vic1); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%+v %v, want the deadline", p, err)
		}
		if d := time.Since(start); d != retryWindow {
			t.Fatalf("gave up after %v, want %v", d, retryWindow)
		}
		if _, zips := f.counts(); zips != 1 {
			t.Fatalf("fetched %d reports", zips)
		}
	})
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

// fakeFoxess is a foxess-local stand-in: it records each call and the
// notices, and reports an override held by holder ("" = none).
type fakeFoxess struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	notices []map[string]any
	holder  string
}

func (f *fakeFoxess) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch r.URL.Path {
	case "/api/v1/state":
		ov := map[string]any{keyActive: false}
		if f.holder != "" {
			ov = map[string]any{keyActive: true, "holder": f.holder, "until": time.Now().Add(time.Hour), "since": time.Now().Add(-time.Hour)}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"online": true, "battery": map[string]any{"soc_pct": 80}, "schedule_override": ov}); err != nil {
			f.t.Error(err)
		}
	case "/api/v1/schedule/override":
		if r.Method == http.MethodPut {
			if body["fd_soc_pct"] != 30.0 || body["fd_pwr_w"] != 5000.0 || body["mode"] != "force-discharge" {
				f.t.Errorf("override body %v", body)
			}
			if err := json.NewEncoder(w).Encode(map[string]any{keyActive: true, "until": body["until"]}); err != nil {
				f.t.Error(err)
			}
		}
	case "/api/v1/notices":
		f.notices = append(f.notices, body)
		w.WriteHeader(http.StatusAccepted)
	}
}

// take returns the calls and notices so far and resets them, with the
// next holder.
func (f *fakeFoxess) take(holder string) (string, []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls, notices := strings.Join(f.calls, ", "), f.notices
	f.calls, f.notices, f.holder = nil, nil, holder
	return calls, notices
}

// TestTickStartsAndNotifies: on a spike the tick puts the override with
// the cut-off and power, then sends one short notice with the price
// (foxess-local notifies the override itself: owner, 2026-11-25); an
// override held by another token is left alone; a stop sends no notice.
func TestTickStartsAndNotifies(t *testing.T) {
	fake := &fakeFoxess{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	now := time.Now()
	s := &sidecar{
		cfg: config{name: sidecarName, region: vic1, powerW: 5000, policy: testPolicy},
		api: &Client{Base: srv.URL, Token: "tok", Client: srv.Client()},
		src: fixedPrice{Price{Region: vic1, RRP: 2000, At: now}},
	}
	s.tick(context.Background(), now)
	calls, notices := fake.take("someone-else")
	if calls != "GET /api/v1/state, PUT /api/v1/schedule/override, POST /api/v1/notices" {
		t.Fatalf("calls: %s", calls)
	}
	if len(notices) != 1 || notices[0]["event"] != "price-spike" || notices[0]["title"] != "Price spike" ||
		notices[0]["message"] != "VIC1 at $2000/MWh." {
		t.Fatalf("start notice: %v", notices)
	}
	s.tick(context.Background(), now)
	if calls, _ := fake.take(sidecarName); calls != "GET /api/v1/state" {
		t.Fatalf("another holder: %v", calls)
	}
	s.src = fixedPrice{Price{Region: vic1, RRP: 50, At: now}}
	s.tick(context.Background(), now)
	if calls, notices := fake.take(""); calls != "GET /api/v1/state, DELETE /api/v1/schedule/override" || len(notices) != 0 {
		t.Fatalf("stop: %s, notices %v", calls, notices)
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
