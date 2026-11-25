package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestOTLPFromEnv: the standard OTEL_* variables. The metrics endpoint
// is used as is; the base endpoint gets /v1/metrics; neither set is off.
// Headers and resource attributes are k=v lists with URL-encoded values;
// OTEL_SERVICE_NAME wins over service.name, which defaults to the name.
func TestOTLPFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c := otlpFromEnv(env(nil), sidecarName)
	if c.Endpoint != "" || c.Resource["service.name"] != sidecarName {
		t.Fatalf("unset: %+v", c)
	}
	c = otlpFromEnv(env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otelcol:4318/",
		"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=Basic%20abc%3D, X-Scope-OrgID = home",
		"OTEL_RESOURCE_ATTRIBUTES":    "service.name=ignored,deployment.environment=home",
		"OTEL_SERVICE_NAME":           "spike",
	}), sidecarName)
	if c.Endpoint != "http://otelcol:4318/v1/metrics" || c.Headers["Authorization"] != "Basic abc=" ||
		c.Headers["X-Scope-OrgID"] != orgID || c.Resource["service.name"] != "spike" ||
		c.Resource["deployment.environment"] != orgID {
		t.Fatalf("base: %+v", c)
	}
	c = otlpFromEnv(env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://ignored:4318",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://otelcol:4318/custom",
		"OTEL_EXPORTER_OTLP_HEADERS":          "a=1",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "b=2",
	}), sidecarName)
	if c.Endpoint != "http://otelcol:4318/custom" || c.Headers["a"] != "" || c.Headers["b"] != "2" {
		t.Fatalf("metrics-specific: %+v", c)
	}
}

// fakeCollector is an OTLP/HTTP receiver: it keeps each body.
type fakeCollector struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	status  int
}

func (f *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := io.ReadAll(r.Body)
	if r.URL.Path != "/v1/metrics" || r.Header.Get("Content-Type") != "application/json" || err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.bodies, f.headers = append(f.bodies, b), append(f.headers, r.Header.Clone())
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
}

func (f *fakeCollector) last(t *testing.T) ([]byte, http.Header) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("nothing exported")
	}
	return f.bodies[len(f.bodies)-1], f.headers[len(f.headers)-1]
}

// The OTLP JSON wire, written out here rather than through the
// encoder's own types, so a misspelt name fails the test.
type (
	wireKV struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	}
	wirePoint struct {
		Attributes        []wireKV `json:"attributes"`
		StartTimeUnixNano string   `json:"startTimeUnixNano"`
		TimeUnixNano      string   `json:"timeUnixNano"`
		AsDouble          *float64 `json:"asDouble"`
		AsInt             string   `json:"asInt"`
	}
	wireData struct {
		DataPoints             []wirePoint `json:"dataPoints"`
		AggregationTemporality int         `json:"aggregationTemporality"`
		IsMonotonic            bool        `json:"isMonotonic"`
	}
	wireBody struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes []wireKV `json:"attributes"`
			} `json:"resource"`
			ScopeMetrics []struct {
				Metrics []struct {
					Name  string    `json:"name"`
					Gauge *wireData `json:"gauge"`
					Sum   *wireData `json:"sum"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
)

// point is one data point, decoded.
type point struct {
	value      float64
	at         time.Time
	attrs      map[string]string
	monotonic  bool
	cumulative bool
}

func wireAttrs(list []wireKV) map[string]string {
	out := map[string]string{}
	for _, a := range list {
		out[a.Key] = a.Value.StringValue
	}
	return out
}

func wireNanos(t *testing.T, s string) time.Time {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return time.Unix(0, n)
}

// decodePoint is one wire point; sum carries a cumulative counter's
// fields (nil for a gauge).
func decodePoint(t *testing.T, name string, p wirePoint, sum *wireData) point {
	t.Helper()
	pt := point{at: wireNanos(t, p.TimeUnixNano), attrs: wireAttrs(p.Attributes)}
	if p.AsDouble != nil {
		pt.value = *p.AsDouble
	} else {
		n, err := strconv.ParseInt(p.AsInt, 10, 64)
		if err != nil {
			t.Fatalf("%s: value %+v", name, p)
		}
		pt.value = float64(n)
	}
	if sum != nil {
		pt.monotonic, pt.cumulative = sum.IsMonotonic, sum.AggregationTemporality == 2
		if p.StartTimeUnixNano == "" {
			t.Fatalf("%s: no start time", name)
		}
	}
	return pt
}

// points indexes an export: metric name → its points, plus the
// resource attributes.
func points(t *testing.T, raw []byte) (map[string][]point, map[string]string) {
	t.Helper()
	var body wireBody
	if err := json.Unmarshal(raw, &body); err != nil || len(body.ResourceMetrics) != 1 {
		t.Fatalf("body %s: %v", raw, err)
	}
	rm := body.ResourceMetrics[0]
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			data, sum := m.Gauge, (*wireData)(nil)
			if data == nil {
				data, sum = m.Sum, m.Sum
			}
			if data == nil {
				t.Fatalf("%s: neither gauge nor sum", m.Name)
			}
			for _, p := range data.DataPoints {
				out[m.Name] = append(out[m.Name], decodePoint(t, m.Name, p, sum))
			}
		}
	}
	return out, wireAttrs(rm.Resource.Attributes)
}

// byAttr is the value of the point with attribute k=v.
func byAttr(t *testing.T, ps []point, k, v string) float64 {
	t.Helper()
	for _, p := range ps {
		if p.attrs[k] == v {
			return p.value
		}
	}
	t.Fatalf("no point with %s=%s in %+v", k, v, ps)
	return 0
}

// override is the two override gauges' values.
func override(t *testing.T, ps map[string][]point) (active, held float64) {
	t.Helper()
	a, h := ps["aemo_spike.override.active"], ps["aemo_spike.override.held"]
	if len(a) != 1 || len(h) != 1 {
		t.Fatalf("override gauges: %+v %+v", a, h)
	}
	return a[0].value, h[0].value
}

const (
	sidecarName = "aemo-spike"
	orgID       = "home"
)

func newMetricsSidecar(t *testing.T, foxessURL string, col *httptest.Server, src PriceSource) *sidecar {
	t.Helper()
	cfg := config{name: sidecarName, region: vic1, powerW: 5000, policy: testPolicy}
	return &sidecar{
		cfg: cfg,
		api: &Client{Base: foxessURL, Token: "tok", Client: http.DefaultClient},
		src: src,
		metrics: newMetrics(otlpConfig{
			Endpoint: col.URL + "/v1/metrics",
			Headers:  map[string]string{"X-Scope-OrgID": orgID},
			Resource: map[string]string{"service.name": sidecarName},
		}, col.Client(), time.Unix(1_700_000_000, 0)),
	}
}

// TestTickExportsMetrics: each tick pushes what it saw and did. The
// price is stamped with its dispatch interval, not the push; the
// override gauges show the state after the tick's own action; the
// thresholds are there to draw; decisions are cumulative counters
// (every action present from the first push, so rate() works at once).
func TestTickExportsMetrics(t *testing.T) {
	fake := &fakeFoxess{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	col := &fakeCollector{}
	cs := httptest.NewServer(col)
	defer cs.Close()
	now := time.Now().Truncate(time.Second)
	at := now.Add(-40 * time.Second)
	s := newMetricsSidecar(t, srv.URL, cs, fixedPrice{Price{Region: vic1, RRP: 2000, At: at}})

	s.tick(context.Background(), now)
	body, hdr := col.last(t)
	if hdr.Get("X-Scope-Orgid") != orgID {
		t.Fatalf("headers: %v", hdr)
	}
	ps, res := points(t, body)
	if res["service.name"] != sidecarName {
		t.Fatalf("resource: %v", res)
	}
	checkFirstPush(t, ps, at, now)

	// another holder: no action, the override is active but not ours
	fake.take("someone-else")
	s.tick(context.Background(), now.Add(5*time.Minute))
	body, _ = col.last(t)
	ps, _ = points(t, body)
	if active, held := override(t, ps); active != 1 || held != 0 {
		t.Fatalf("another holder: %v %v", active, held)
	}
	if byAttr(t, ps["aemo_spike.decisions"], "action", "start") != 1 {
		t.Fatalf("counters are cumulative: %+v", ps["aemo_spike.decisions"])
	}
}

// checkFirstPush: the first push after a spike started the override.
func checkFirstPush(t *testing.T, ps map[string][]point, at, now time.Time) {
	t.Helper()
	price := ps["aemo.dispatch.price"]
	if len(price) != 1 || price[0].value != 2000 || !price[0].at.Equal(at) || price[0].attrs["region"] != vic1 {
		t.Fatalf("price: %+v", price)
	}
	if soc := ps["aemo_spike.battery.soc"]; len(soc) != 1 || soc[0].value != 80 || !soc[0].at.Equal(now) {
		t.Fatalf("soc: %+v", soc)
	}
	if active, held := override(t, ps); active != 1 || held != 1 {
		t.Fatalf("override after a start: %v %v", active, held)
	}
	checkThresholdsAndDecisions(t, ps)
}

// checkThresholdsAndDecisions: the policy's lines, and one start.
func checkThresholdsAndDecisions(t *testing.T, ps map[string][]point) {
	t.Helper()
	th := ps["aemo_spike.threshold"]
	if byAttr(t, th, "kind", "start") != testPolicy.StartAbove || byAttr(t, th, "kind", "stop") != testPolicy.StopBelow {
		t.Fatalf("thresholds: %+v", th)
	}
	dec := ps["aemo_spike.decisions"]
	if byAttr(t, dec, "action", "start") != 1 || byAttr(t, dec, "action", "hold") != 0 ||
		byAttr(t, dec, "action", "stop") != 0 || !dec[0].monotonic || !dec[0].cumulative {
		t.Fatalf("decisions: %+v", dec)
	}
}

type failingPrice struct{}

func (failingPrice) Latest(context.Context, string) (Price, error) {
	return Price{}, errors.New("nemweb down")
}

// TestTickExportsErrors: a tick that cannot read foxess-local or the
// price still pushes, counting the failure by source; a collector that
// refuses the push changes nothing the tick does.
func TestTickExportsErrors(t *testing.T) {
	col := &fakeCollector{}
	cs := httptest.NewServer(col)
	defer cs.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	s := newMetricsSidecar(t, down.URL, cs, failingPrice{})
	s.tick(context.Background(), time.Now())
	body, _ := col.last(t)
	ps, _ := points(t, body)
	if byAttr(t, ps["aemo_spike.errors"], "source", "foxess") != 1 || byAttr(t, ps["aemo_spike.errors"], "source", "nemweb") != 0 {
		t.Fatalf("errors: %+v", ps["aemo_spike.errors"])
	}
	if ps["aemo.dispatch.price"] != nil || ps["aemo_spike.battery.soc"] != nil {
		t.Fatalf("nothing seen, nothing claimed: %+v", ps)
	}

	fake := &fakeFoxess{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	col.status = http.StatusInternalServerError
	s = newMetricsSidecar(t, srv.URL, cs, failingPrice{})
	s.tick(context.Background(), time.Now())
	body, _ = col.last(t)
	ps, _ = points(t, body)
	if byAttr(t, ps["aemo_spike.errors"], "source", "nemweb") != 1 {
		t.Fatalf("errors: %+v", ps["aemo_spike.errors"])
	}
	if calls, _ := fake.take(""); calls != "GET /api/v1/state" {
		t.Fatalf("calls: %s", calls)
	}
}
