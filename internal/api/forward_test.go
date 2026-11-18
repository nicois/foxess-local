package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestForwardModeSet: PUT (installer) changes the seam, responds with
// the new mode, publishes the system event, and writes the audit row.
func TestForwardModeSet(t *testing.T) {
	h := newHarness(t)
	sub := h.bus.Subscribe()
	defer sub.Close()

	rr := h.do(t, httpPut, pathForwardMode, RoleInstaller, map[string]string{jsonFieldMode: forwardModeFull})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /forward-mode: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Mode != forwardModeFull {
		t.Fatalf("response mode: got %q, want full", body.Mode)
	}
	// The seam reflects the change.
	if mode, _, err := h.fwd.Get(); err != nil || string(mode) != forwardModeFull {
		t.Fatalf("seam: got %q err=%v, want full", mode, err)
	}
	// /state carries it.
	rr = h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("state body: %v", err)
	}
	if st.ForwardMode != forwardModeFull {
		t.Fatalf("state forward_mode: got %q, want full", st.ForwardMode)
	}
	// The bus carries the spec's system event.
	sys := awaitSystemEvent(t, sub.Events(), "forward-mode-changed")
	if !contains(sys.Detail, forwardModeFull) {
		t.Fatalf("event detail: got %q, want it to name the new mode", sys.Detail)
	}
	// The store gains the change audit row (source = acting token name).
	h.st.Flush()
	evts, err := h.st.QueryAudit(time.Time{}, "forward-mode")
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(evts) != 1 || !contains(evts[0].Detail, forwardModeFull) || !contains(evts[0].Detail, "tok-installer") {
		t.Fatalf("audit rows: got %+v, want one forward-mode change by tok-installer", evts)
	}
}

// TestForwardModeInvalid: an unknown mode is 400 invalid_value.
func TestForwardModeInvalid(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPut, pathForwardMode, RoleInstaller, map[string]string{jsonFieldMode: "turbo"})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != "mode" {
		t.Fatalf("invalid mode: got errors %+v, want field mode", p.Errors)
	}
}

// TestConfigView: the read-only config view carries the spec's required
// fields plus the forward mode (no secrets).
func TestConfigView(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, "/api/v1/config", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	rawHours, ok := body["raw_hours"].(float64)
	if !ok || int(rawHours) != defaultRawHours {
		t.Fatalf("raw_hours: got %v, want %d", body["raw_hours"], defaultRawHours)
	}
	bucketDays, ok := body["bucket_days"].(float64)
	if !ok || int(bucketDays) != defaultBucketDays {
		t.Fatalf("bucket_days: got %v, want %d", body["bucket_days"], defaultBucketDays)
	}
	if body["version"] == nil || body["version"] == "" {
		t.Fatalf("version: got %v, want a non-empty string", body["version"])
	}
	if body["forward_mode"] != forwardModeTelemetryOnly {
		t.Fatalf("forward_mode: got %v, want telemetry_only", body["forward_mode"])
	}
}

// TestConfigClock (audit R8): /config names the host time zone the
// schedule windows are evaluated in and, once the inverter's clock
// (0x9C40 ×6, year…second) has been read, how far it is from the host's.
func TestConfigClock(t *testing.T) {
	h := newHarness(t)
	var body struct {
		TimeZone    string   `json:"time_zone"`
		UTCOffsetS  *int     `json:"utc_offset_s"`
		ClockOffset *float64 `json:"inverter_clock_offset_s"`
	}
	get := func() {
		rr := h.do(t, httpGet, "/api/v1/config", RoleReadOnly, nil)
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	get()
	if body.TimeZone == "" || body.UTCOffsetS == nil || body.ClockOffset != nil {
		t.Fatalf("before an RTC read: %+v", body)
	}
	rtc := testBase.In(time.Local).Add(90 * time.Second) // inverter 90 s fast
	//nolint:gosec // G115: calendar fields of a 2026 date fit uint16
	h.st.RecordRegisters(testBase, 40000, []uint16{
		uint16(rtc.Year()), uint16(rtc.Month()), uint16(rtc.Day()),
		uint16(rtc.Hour()), uint16(rtc.Minute()), uint16(rtc.Second()),
	})
	h.st.Flush()
	get()
	if body.ClockOffset == nil || *body.ClockOffset != 90 {
		t.Fatalf("inverter_clock_offset_s: %v, want 90", body.ClockOffset)
	}
}
