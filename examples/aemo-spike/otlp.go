package main

// Optional metrics: each tick pushes what it saw and did to an
// OpenTelemetry collector over OTLP/HTTP (JSON), so the price can be
// graphed beside the battery in Grafana (otelcol → Mimir / Prometheus).
// foxess-local stays out of it. Standard library only: the OTel SDK
// would multiply the binary and its memory on an OpenStick, for one
// small request every 5 minutes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// otlpConfig is where and how to push; an empty Endpoint is off.
type otlpConfig struct {
	Endpoint string            // the full URL, …/v1/metrics
	Headers  map[string]string // e.g. Authorization, X-Scope-OrgID
	Resource map[string]string // resource attributes; service.name is always set
}

// otlpFromEnv reads the standard exporter variables: the metrics
// endpoint as is, else the base endpoint + /v1/metrics; the
// metrics-specific headers, else the general ones; the resource
// attributes, with OTEL_SERVICE_NAME (else the sidecar's name) as
// service.name.
func otlpFromEnv(getenv func(string) string, name string) otlpConfig {
	c := otlpConfig{Endpoint: getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")}
	if c.Endpoint == "" {
		if base := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			c.Endpoint = strings.TrimRight(base, "/") + "/v1/metrics"
		}
	}
	h := getenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS")
	if h == "" {
		h = getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	c.Headers = parseKV(h)
	c.Resource = parseKV(getenv("OTEL_RESOURCE_ATTRIBUTES"))
	switch sn := getenv("OTEL_SERVICE_NAME"); {
	case sn != "":
		c.Resource["service.name"] = sn
	case c.Resource["service.name"] == "":
		c.Resource["service.name"] = name
	}
	return c
}

// parseKV parses "k=v,k=v" with URL-encoded values (the OTEL_* list
// format); a malformed entry is skipped.
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for item := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(item, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		if dv, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			out[k] = dv
		}
	}
	return out
}

// Error sources, counted by aemo_spike.errors.
const (
	errFoxess   = "foxess"   // GET /state failed
	errNEMWeb   = "nemweb"   // no price
	errOverride = "override" // a start or clear was refused
)

var (
	actionNames  = []string{actionName[Hold], actionName[Start], actionName[Stop]}
	errorSources = []string{errFoxess, errNEMWeb, errOverride}
)

// observation is what one tick saw and did.
type observation struct {
	now            time.Time
	price          *Price
	socPct         int  // -1 = unknown
	known          bool // the override state was read
	overrideActive bool
	held           bool   // held by this sidecar, after the tick's action
	action         string // "" when the tick did not decide
	errs           []string
}

// Metrics pushes observations. Counters are cumulative from start, so
// a lost push loses nothing but its gauges, which the next one resends.
type Metrics struct {
	cfg       otlpConfig
	client    *http.Client
	start     time.Time
	decisions map[string]int64
	errors    map[string]int64
}

func newMetrics(cfg otlpConfig, client *http.Client, start time.Time) *Metrics {
	m := &Metrics{cfg: cfg, client: client, start: start, decisions: map[string]int64{}, errors: map[string]int64{}}
	for _, a := range actionNames {
		m.decisions[a] = 0
	}
	for _, e := range errorSources {
		m.errors[e] = 0
	}
	return m
}

// otlpExportLimit bounds a collector's reply (only drained).
const otlpExportLimit = 64 << 10

// Export counts obs and pushes the result. The tick is single-threaded,
// so the counters need no lock.
func (m *Metrics) Export(ctx context.Context, obs observation, p Policy) error {
	if obs.action != "" {
		m.decisions[obs.action]++
	}
	for _, e := range obs.errs {
		m.errors[e]++
	}
	body, err := json.Marshal(m.request(obs, p))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range m.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, otlpExportLimit))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector: HTTP %d", resp.StatusCode)
	}
	return nil
}

// The OTLP/HTTP JSON encoding (opentelemetry-proto, metrics v1): 64-bit
// integers as strings, enums as numbers.
type (
	otlpRequest struct {
		ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
	}
	otlpResourceMetrics struct {
		Resource     otlpResource       `json:"resource"`
		ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
	}
	otlpResource struct {
		Attributes []otlpKV `json:"attributes"`
	}
	otlpScopeMetrics struct {
		Scope   otlpScope    `json:"scope"`
		Metrics []otlpMetric `json:"metrics"`
	}
	otlpScope struct {
		Name string `json:"name"`
	}
	otlpMetric struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		Unit        string     `json:"unit"`
		Gauge       *otlpGauge `json:"gauge,omitempty"`
		Sum         *otlpSum   `json:"sum,omitempty"`
	}
	otlpGauge struct {
		DataPoints []otlpPoint `json:"dataPoints"`
	}
	otlpSum struct {
		DataPoints             []otlpPoint `json:"dataPoints"`
		AggregationTemporality int         `json:"aggregationTemporality"` // 2 = cumulative
		IsMonotonic            bool        `json:"isMonotonic"`
	}
	otlpPoint struct {
		Attributes        []otlpKV `json:"attributes,omitempty"`
		StartTimeUnixNano string   `json:"startTimeUnixNano,omitempty"`
		TimeUnixNano      string   `json:"timeUnixNano"`
		AsDouble          *float64 `json:"asDouble,omitempty"`
		AsInt             string   `json:"asInt,omitempty"`
	}
	otlpKV struct {
		Key   string       `json:"key"`
		Value otlpAnyValue `json:"value"`
	}
	otlpAnyValue struct {
		StringValue string `json:"stringValue"`
	}
)

const (
	cumulative = 2
	unitPrice  = "AUD/MWh"
)

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// kv is attributes from a map, sorted by key (a stable body).
func kv(m map[string]string) []otlpKV {
	out := make([]otlpKV, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, otlpKV{Key: k, Value: otlpAnyValue{StringValue: m[k]}})
	}
	return out
}

func gauge(name, desc, unit string, pts ...otlpPoint) otlpMetric {
	return otlpMetric{Name: name, Description: desc, Unit: unit, Gauge: &otlpGauge{DataPoints: pts}}
}

func dbl(at time.Time, v float64, attrs map[string]string) otlpPoint {
	return otlpPoint{Attributes: kv(attrs), TimeUnixNano: nanos(at), AsDouble: &v}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// counter is a cumulative monotonic sum, one point per key of counts.
func (m *Metrics) counter(name, desc, attr string, keys []string, counts map[string]int64, now time.Time) otlpMetric {
	pts := make([]otlpPoint, 0, len(keys))
	for _, k := range keys {
		pts = append(pts, otlpPoint{
			Attributes:        kv(map[string]string{attr: k}),
			StartTimeUnixNano: nanos(m.start),
			TimeUnixNano:      nanos(now),
			AsInt:             strconv.FormatInt(counts[k], 10),
		})
	}
	return otlpMetric{
		Name: name, Description: desc, Unit: "{" + attr + "}",
		Sum: &otlpSum{DataPoints: pts, AggregationTemporality: cumulative, IsMonotonic: true},
	}
}

// request builds one export. A value the tick did not see is left out
// rather than sent as zero.
func (m *Metrics) request(obs observation, p Policy) otlpRequest {
	now := obs.now
	var ms []otlpMetric
	if pr := obs.price; pr != nil {
		// stamped with the dispatch interval (its end), not the push
		ms = append(ms, gauge("aemo.dispatch.price", "The NEM dispatch price (RRP) for the region", unitPrice,
			dbl(pr.At, pr.RRP, map[string]string{"region": pr.Region})))
	}
	if obs.socPct >= 0 {
		ms = append(ms, gauge("aemo_spike.battery.soc", "The battery SoC read from foxess-local", "%",
			dbl(now, float64(obs.socPct), nil)))
	}
	if obs.known {
		ms = append(ms,
			gauge("aemo_spike.override.active", "1 while any schedule override is active", "",
				dbl(now, b2f(obs.overrideActive), nil)),
			gauge("aemo_spike.override.held", "1 while this sidecar holds the override", "",
				dbl(now, b2f(obs.held), nil)))
	}
	ms = append(ms,
		gauge("aemo_spike.threshold", "The policy's start / stop prices", unitPrice,
			dbl(now, p.StartAbove, map[string]string{"kind": "start"}), dbl(now, p.StopBelow, map[string]string{"kind": "stop"})),
		m.counter("aemo_spike.decisions", "Decisions by action", "action", actionNames, m.decisions, now),
		m.counter("aemo_spike.errors", "Failed steps by source", "source", errorSources, m.errors, now))
	return otlpRequest{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     otlpResource{Attributes: kv(m.cfg.Resource)},
		ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "aemo-spike"}, Metrics: ms}},
	}}}
}
