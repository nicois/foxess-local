package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"foxesslocal/internal/config"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/settings"
)

const (
	pathSettings      = "/api/v1/settings"
	pathSetup         = "/api/v1/setup"
	pathSetupComplete = "/api/v1/setup/complete"
)

// withSettings wires live settings into the harness (defaults: public
// access on, forward mode telemetry_only, capacity unset).
func withSettings(t *testing.T, h *harness) *settings.Settings {
	t.Helper()
	cfg := &config.Config{}
	cfg.Access.PublicReadOnly = true
	cfg.Forward.Mode = policy.ModeTelemetryOnly
	set, err := settings.New(h.st, cfg, h.clk.get)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.info.Settings = set
	return set
}

type settingsBody struct {
	Settings []struct {
		Key    string `json:"key"`
		Value  any    `json:"value"`
		Source string `json:"source"`
	} `json:"settings"`
}

func (b settingsBody) find(key string) (any, string) {
	for _, s := range b.Settings {
		if s.Key == key {
			return s.Value, s.Source
		}
	}
	return "missing", ""
}

// TestSettingsGetPut (F12): the effective settings with their sources;
// an installer sets and resets them; the API reflects them live.
func TestSettingsGetPut(t *testing.T) {
	h := newHarness(t)
	withSettings(t, h)
	var b settingsBody
	decode(t, h.do(t, httpGet, pathSettings, RoleReadOnly, nil), &b)
	if v, src := b.find(settings.KeyCapacityWh); v != nil || src != string(settings.SourceDefault) {
		t.Fatalf("capacity before: %v %s", v, src)
	}

	rr := h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{
		settings.KeyCapacityWh: 41930, settings.KeyPublicAllPages: true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	decode(t, rr, &b)
	if v, src := b.find(settings.KeyCapacityWh); v != float64(41930) || src != string(settings.SourceUI) {
		t.Fatalf("capacity after: %v %s", v, src)
	}
	var cfgView map[string]any
	decode(t, h.do(t, httpGet, "/api/v1/config", RoleReadOnly, nil), &cfgView)
	if cfgView["public_all_pages"] != true {
		t.Fatalf("/config does not follow the setting: %v", cfgView["public_all_pages"])
	}

	// null resets to the file / default
	decode(t, h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyCapacityWh: nil}), &b)
	if v, _ := b.find(settings.KeyCapacityWh); v != nil {
		t.Fatalf("capacity after reset: %v", v)
	}
}

// TestSettingsPutRefused: invalid values and forward mode (which has its
// own endpoint) are refused with the field named; nothing changes.
func TestSettingsPutRefused(t *testing.T) {
	h := newHarness(t)
	set := withSettings(t, h)
	for _, body := range []map[string]any{
		{settings.KeyCapacityWh: -5},
		{settings.KeyCapacityWh: "lots"},
		{settings.KeyForwardMode: "full"},
		{"nonsense": 1},
		{settings.KeyCapacityWh: 41930, settings.KeyPublicReadOnly: "yes"},
	} {
		p := checkProblem(t, h.do(t, httpPut, pathSettings, RoleInstaller, body), http.StatusBadRequest, "invalid_value")
		if len(p.Errors) == 0 {
			t.Fatalf("%v: no field named", body)
		}
	}
	if set.Get().CapacityWh != 0 {
		t.Fatal("a refused request changed a setting")
	}
}

type setupBody struct {
	Completed bool `json:"completed"`
	Checks    []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
		Fix    string `json:"fix"`
	} `json:"checks"`
	Battery struct {
		Modules    *int `json:"modules"`
		CapacityWh *int `json:"capacity_wh"`
	} `json:"battery"`
}

func (b setupBody) status(id string) string {
	for _, c := range b.Checks {
		if c.ID == id {
			return c.Status
		}
	}
	return "missing"
}

// TestSetupChecklist (F12): a fresh install — nothing connected yet —
// fails the connection check with a fix, and asks for the capacity;
// with data flowing and the capacity set, those checks pass.
func TestSetupChecklist(t *testing.T) {
	h := newHarness(t)
	withSettings(t, h)
	var b setupBody
	decode(t, h.do(t, httpGet, pathSetup, RoleInstaller, nil), &b)
	if b.Completed || b.status("inverter_connected") != checkFail || b.status("battery_capacity") != checkFail {
		t.Fatalf("fresh install: %+v", b)
	}
	for _, c := range b.Checks {
		if c.Status == checkFail && c.Fix == "" {
			t.Fatalf("check %s fails without a fix", c.ID)
		}
	}

	h.seedFullState()
	h.seedFrame(testBase, dirC2S, "snap01", decisionForwarded, snap01Fixture("1.73", "1.03", "1.71", "0.00"), nil)
	h.st.Flush()
	h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyCapacityWh: 41930})
	var after setupBody
	decode(t, h.do(t, httpGet, pathSetup, RoleInstaller, nil), &after)
	if after.status("battery_capacity") != "ok" || after.status("identified") != "ok" || after.status("verified") != "ok" {
		t.Fatalf("after data and capacity: %+v", after)
	}
	for _, c := range after.Checks {
		if c.Status == "ok" && c.Fix != "" {
			t.Fatalf("check %s passes but carries a fix", c.ID)
		}
	}
	if after.Battery.CapacityWh == nil || *after.Battery.CapacityWh != 41930 {
		t.Fatalf("battery: %+v", after.Battery)
	}
}

// TestSetupComplete: completion is recorded and reported.
func TestSetupComplete(t *testing.T) {
	h := newHarness(t)
	withSettings(t, h)
	if rr := h.do(t, httpPost, pathSetupComplete, RoleInstaller, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("complete: %d %s", rr.Code, rr.Body.String())
	}
	var b setupBody
	decode(t, h.do(t, httpGet, pathSetup, RoleInstaller, nil), &b)
	if !b.Completed {
		t.Fatal("completion not reported")
	}
}

func decode(t *testing.T, rr interface{ Result() *http.Response }, dst any) {
	t.Helper()
	res := rr.Result()
	defer func() { _ = res.Body.Close() }()
	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		t.Fatalf("decode (%d): %v", res.StatusCode, err)
	}
}

// TestSettingsNotifyTargets (F2): an installer sets the notification
// URLs; "" turns a target off (over the file's value), null resets it.
// The URL is a secret: only an installer reads it back (others see that
// it is set), it never enters the audit log, and a non-http(s) value is
// refused.
func TestSettingsNotifyTargets(t *testing.T) {
	h := newHarness(t)
	set := withSettings(t, h)
	const hook = "http://ha.example:8123/api/webhook/secret-id"
	rec := h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyNotifyWebhook: hook})
	if rec.Code != http.StatusOK || set.Get().NotifyWebhook != hook {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	var b settingsBody
	decode(t, h.do(t, httpGet, pathSettings, RoleInstaller, nil), &b)
	if v, src := b.find(settings.KeyNotifyWebhook); v != hook || src != string(settings.SourceUI) {
		t.Fatalf("installer view: %v %s", v, src)
	}
	decode(t, h.do(t, httpGet, pathSettings, RoleReadOnly, nil), &b)
	if v, _ := b.find(settings.KeyNotifyWebhook); v != "(set)" {
		t.Fatalf("read-only view: %v", v)
	}
	if v, _ := b.find(settings.KeyNotifyNtfy); v != nil {
		t.Fatalf("unset ntfy: %v", v)
	}
	if rec := h.do(t, httpGet, "/api/v1/audit", RoleReadOnly, nil); strings.Contains(rec.Body.String(), "secret-id") {
		t.Fatalf("the URL reached the audit log: %s", rec.Body.String())
	}
	if rec := h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyNotifyNtfy: "ftp://x/y"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("ftp URL: %d", rec.Code)
	}
	h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyNotifyWebhook: ""})
	if set.Get().NotifyWebhook != "" {
		t.Fatalf("off: %q", set.Get().NotifyWebhook)
	}
	decode(t, h.do(t, httpGet, pathSettings, RoleInstaller, nil), &b)
	if v, src := b.find(settings.KeyNotifyWebhook); v != nil || src != string(settings.SourceUI) {
		t.Fatalf("turned off: %v %s", v, src)
	}
	h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeyNotifyWebhook: nil})
	decode(t, h.do(t, httpGet, pathSettings, RoleInstaller, nil), &b)
	if _, src := b.find(settings.KeyNotifyWebhook); src != string(settings.SourceDefault) {
		t.Fatalf("reset: %s", src)
	}
}
