package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/store"
)

// seedAuditFrames seeds the mixed-decision frame set every audit test
// uses: an allowed cloud read and a denied write (the two s2c policy
// decisions — the hold is retired).
func (h *harness) seedAuditFrames() {
	// An hour before the clock: the operator command row (written at
	// the clock) must be strictly the newest entry.
	t0 := testBase.Add(-time.Hour)
	// Allowed: the cloud's 48010 read (forwarded s2c).
	h.seedFrame(t0, dirS2C, classMbread, decisionForwarded,
		[]byte{0x01, 0x03, 0xBB, 0x8A, 0x00, 0x50}, nil)
	// Denied: the cloud's work-mode write (dropped s2c, C-001).
	h.seedFrame(t0.Add(time.Second), dirS2C, classMbWrite, decisionDropped,
		fn06Pdu(regWorkModeAddr, 1), nil)
}

// TestAuditEntries: the two policy decisions (allow / drop, source
// cloud) plus the operator command row (source = token name, confirmed
// yes) — newest first.
// assertAuditCommandRow checks the operator command row (source = the
// token name, allow, confirmed yes, the work-mode summary, ts at the
// clock).
func assertAuditCommandRow(t *testing.T, e auditEntryBody) {
	t.Helper()
	if e.Source != "tok-standard" || e.Action != auditActionAllow || e.Confirmed != confirmedYes {
		t.Fatalf("command entry: got source=%q action=%q confirmed=%q", e.Source, e.Action, e.Confirmed)
	}
	if e.SourceRole == nil || *e.SourceRole != string(RoleStandard) {
		t.Fatalf("command source_role: got %v, want standard", e.SourceRole)
	}
	if e.TS.Before(testBase) {
		t.Fatalf("command ts: got %s, want >= %s", e.TS, testBase)
	}
	if !contains(e.Summary, "work-mode") || !contains(e.Summary, workModeFeedIn) {
		t.Fatalf("command summary: got %q, want a work-mode feed-in line", e.Summary)
	}
}

// assertAuditPolicyRows checks the two cloud policy rows in order
// (drop, allow), source cloud, no source role.
func assertAuditPolicyRows(t *testing.T, rows []auditEntryBody) {
	t.Helper()
	drop, allow := rows[0], rows[1]
	if drop.Source != auditSourceCloud || drop.Action != auditActionDrop || drop.SourceRole != nil {
		t.Fatalf("dropped entry: got source=%q action=%q role=%v", drop.Source, drop.Action, drop.SourceRole)
	}
	if !contains(drop.Summary, "work-mode") {
		t.Fatalf("dropped summary: got %q, want the register summary", drop.Summary)
	}
	if allow.Source != auditSourceCloud || allow.Action != auditActionAllow || allow.Confirmed != confirmedNotApplicable {
		t.Fatalf("allowed entry: got source=%q action=%q confirmed=%q",
			allow.Source, allow.Action, allow.Confirmed)
	}
}

func TestAuditEntries(t *testing.T) {
	h := newHarness(t)
	h.seedAuditFrames()
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /work-mode: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	rr = h.do(t, httpGet, "/api/v1/audit", RoleExpert, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /audit: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Items []auditEntryBody `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Items) != 3 {
		t.Fatalf("audit: got %d entries, want 3 (2 policy + 1 command)", len(body.Items))
	}
	// Newest first: the command (just written) leads.
	assertAuditCommandRow(t, body.Items[0])
	// The policy rows follow, source cloud, source_role null.
	assertAuditPolicyRows(t, body.Items[1:])
}

// TestAuditFilters: the action and source filters each select exactly
// their set.
func TestAuditFilters(t *testing.T) {
	h := newHarness(t)
	h.seedAuditFrames()
	h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})

	rr := h.do(t, httpGet, "/api/v1/audit?action=drop", RoleExpert, nil)
	var body struct {
		Items []auditEntryBody `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Action != auditActionDrop {
		t.Fatalf("action=drop: got %+v, want exactly the drop", body.Items)
	}

	rr = h.do(t, httpGet, "/api/v1/audit?source=cloud", RoleExpert, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("source=cloud: got %d entries, want 2", len(body.Items))
	}
	for _, e := range body.Items {
		if e.Source != auditSourceCloud {
			t.Fatalf("source=cloud: entry leaked source %q", e.Source)
		}
	}
}

// auditEntryBody mirrors the spec's AuditEntry schema.
type auditEntryBody struct {
	TS         time.Time `json:"ts"`
	Source     string    `json:"source"`
	SourceRole *string   `json:"source_role"`
	Action     string    `json:"action"`
	Summary    string    `json:"summary"`
	Confirmed  string    `json:"confirmed"`
}

// TestMessages: the raw frame log with dir and frame filters, the hex
// and the decoded summary, newest first, limit and before paging.
// messagesGet drives GET /api/v1/messages as read-only with the given
// query and returns the decoded items.
func (h *harness) messagesGet(t *testing.T, q string) []messageEntryBody {
	t.Helper()
	rr := h.do(t, httpGet, "/api/v1/messages"+q, RoleExpert, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /messages: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Items []messageEntryBody `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	return body.Items
}

// assertOldestMbwrite checks one entry against an mbwrite PDU (frame
// type mapped per the spec enum, length, hex) and the seeded header
// (seedFrame's device and fn: the device bytes carry the sender's clock,
// which a classifier must not key on — profile §3).
func assertOldestMbwrite(t *testing.T, e messageEntryBody, pdu []byte) {
	t.Helper()
	if e.Frame != "modbus-write" || e.Length != len(pdu) {
		t.Fatalf("oldest: got frame=%q length=%d", e.Frame, e.Length)
	}
	if want := hex.EncodeToString(pdu); e.Hex != want {
		t.Fatalf("hex: got %q, want %q", e.Hex, want)
	}
	if e.Device != "aa6b0119" || e.Fn != 0x10 {
		t.Fatalf("header: got device=%q fn=%#x, want aa6b0119 0x10", e.Device, e.Fn)
	}
}

func TestMessages(t *testing.T) {
	h := newHarness(t)
	t0 := testBase
	// Three mbwrite frames (cloud → inverter) at increasing ts, one with
	// a decoded summary.
	decoded := json.RawMessage(`{"fn":6,"addr":41000,"value":1}`)
	h.seedFrame(t0, dirS2C, classMbWrite, decisionForwarded, fn06Pdu(regImportLimAddr, 5000), nil)
	h.seedFrame(t0.Add(time.Second), dirS2C, classMbWrite, decisionDropped, fn06Pdu(regExportLimAddr, 3000), nil)
	h.seedFrame(t0.Add(2*time.Second), dirS2C, classMbWrite, decisionForwarded, fn06Pdu(regWorkModeAddr, 1), decoded)
	// One inverter → cloud telemetry frame.
	h.seedTele(t0.Add(3*time.Second), 100, 50, 60, 40, 10, 42, 4)

	items := h.messagesGet(t, "")
	if len(items) != 4 {
		t.Fatalf("messages: got %d entries, want 4", len(items))
	}
	// Newest first: the tele leads, its class mapped per the spec enum.
	if items[0].Dir != dirInverterToCloud || items[0].Frame != "telemetry-150" {
		t.Fatalf("newest: got dir=%q frame=%q, want inverter-to-cloud telemetry-150",
			items[0].Dir, items[0].Frame)
	}
	// The decoded summary is present where the frame has it.
	if items[1].Summary == nil || !contains(*items[1].Summary, "addr") {
		t.Fatalf("summary: got %v, want the decoded JSON", items[1].Summary)
	}
	if items[2].Summary != nil {
		t.Fatalf("summary: got %v, want null (no decoded JSON)", items[2].Summary)
	}
	// hex and length are the payload's.
	assertOldestMbwrite(t, items[3], fn06Pdu(regImportLimAddr, 5000))

	items = h.messagesGet(t, "?dir="+dirInverterToCloud)
	if len(items) != 1 || items[0].Dir != dirInverterToCloud {
		t.Fatalf("dir filter: got %+v, want only the c2s frame", items)
	}

	items = h.messagesGet(t, "?frame=modbus-write")
	if len(items) != 3 {
		t.Fatalf("frame filter: got %d entries, want 3 mbwrite", len(items))
	}

	// limit + before paging: two newest mbwrite, then the next page.
	items = h.messagesGet(t, "?frame=modbus-write&limit=2")
	if len(items) != 2 || items[0].TS != t0.Add(2*time.Second) {
		t.Fatalf("limit page: got %+v", items)
	}
	before := t0.Add(2 * time.Second).Format(time.RFC3339Nano)
	items = h.messagesGet(t, "?frame=modbus-write&limit=2&before="+before)
	if len(items) != 2 || items[0].TS != t0.Add(time.Second) {
		t.Fatalf("before page: got %+v, want the two older mbwrite", items)
	}
}

// TestMessagesBadFilter: an unknown dir or frame value is 400.
func TestMessagesBadFilter(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"dir=both", "frame=bogus"} {
		rr := h.do(t, httpGet, "/api/v1/messages?"+q, RoleExpert, nil)
		p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
		if len(p.Errors) == 0 {
			t.Fatalf("%s: want errors[], got none", q)
		}
	}
}

// messageEntryBody mirrors the spec's MessageEntry schema.
type messageEntryBody struct {
	TS      time.Time `json:"ts"`
	Dir     string    `json:"dir"`
	Frame   string    `json:"frame"`
	Length  int       `json:"length"`
	Hex     string    `json:"hex"`
	Device  string    `json:"device"`
	Fn      int       `json:"fn"`
	Summary *string   `json:"summary"`
}

// TestMessagesDecisionReasonOrigin: each message says what happened to it
// and why — the decision, the policy or routing reason, and who injected
// it — so the frame log shows, e.g., a cloud read relayed in
// telemetry_only, a denied write, and an answer kept from the cloud.
func TestMessagesDecisionReasonOrigin(t *testing.T) {
	h := newHarness(t)
	seed := func(ts time.Time, dir capture.Dir, class frame.Kind, decision store.Decision, reason, origin string) {
		h.st.RecordFrame(store.FrameRecord{
			ConnID: testConnID, Dir: dir, TS: ts, Class: class, Fn: 0x10, Plen: 6,
			Payload: []byte{1, 3, 0xbb, 0x80, 0, 10}, Decision: decision, Reason: reason, Origin: origin,
		})
	}
	seed(testBase, capture.DirS2C, classMbread, "forwarded", "mode:telemetry_only read:kind=mbread fn=0x03 addr=48000 count=10", "")
	seed(testBase.Add(time.Second), capture.DirS2C, classMbread, "injected", "", "gateway")
	seed(testBase.Add(2*time.Second), capture.DirC2S, "cfg91", "forwarded", "answer to this app's request: kept from the cloud", "")
	h.st.Flush()

	var body struct {
		Items []struct {
			Decision string  `json:"decision"`
			Reason   *string `json:"reason"`
			Origin   *string `json:"origin"`
		} `json:"items"`
	}
	rr := h.do(t, httpGet, "/api/v1/messages", RoleExpert, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 3 {
		t.Fatalf("%d items", len(body.Items))
	}
	// newest first
	kept, injected, relayed := body.Items[0], body.Items[1], body.Items[2]
	if kept.Decision != "forwarded" || kept.Reason == nil || *kept.Reason != "answer to this app's request: kept from the cloud" || kept.Origin != nil {
		t.Errorf("kept answer: %+v", kept)
	}
	if injected.Decision != "injected" || injected.Reason != nil || injected.Origin == nil || *injected.Origin != "gateway" {
		t.Errorf("injected: %+v", injected)
	}
	if relayed.Reason == nil || *relayed.Reason == "" {
		t.Errorf("relayed read: %+v", relayed)
	}
}

// TestLowLevelNeedsExpert (owner, 2026-11-19): below expert there is no
// frame-level data. The raw frame log refuses a read-only token (and a
// tokenless public session); Activity shows a read-only token the
// operator actions only, without the per-frame cloud decisions, and
// says they are hidden.
func TestLowLevelNeedsExpert(t *testing.T) {
	h := newHarness(t)
	h.seedAuditFrames()
	h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: workModeFeedIn})

	checkProblem(t, h.do(t, httpGet, "/api/v1/messages", RoleReadOnly, nil), http.StatusForbidden, "insufficient_role")

	type page struct {
		Items       []auditEntryBody `json:"items"`
		CloudHidden bool             `json:"cloud_hidden"`
	}
	get := func(role Role, q string) page {
		t.Helper()
		rr := h.do(t, httpGet, "/api/v1/audit"+q, role, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s GET /audit%s: %d", role, q, rr.Code)
		}
		var p page
		if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, q := range []string{"", "?source=cloud", "?action=drop"} {
		p := get(RoleReadOnly, q)
		if !p.CloudHidden {
			t.Errorf("read-only %q: cloud_hidden false", q)
		}
		for _, e := range p.Items {
			if e.Source == auditSourceCloud {
				t.Errorf("read-only %q: a cloud decision leaked: %+v", q, e)
			}
		}
	}
	if p := get(RoleReadOnly, ""); len(p.Items) != 1 {
		t.Fatalf("read-only: %d entries, want the one operator action", len(p.Items))
	}
	if p := get(RoleExpert, ""); p.CloudHidden || len(p.Items) != 3 {
		t.Fatalf("expert: hidden %v, %d entries, want all 3", p.CloudHidden, len(p.Items))
	}
}

// TestSummarizeFirmware (owner, 2026-11-19): Activity names a cloud
// firmware update — the file and the component — instead of "other"
// (the URL stays on the expert-only Messages page).
func TestSummarizeFirmware(t *testing.T) {
	task := frame.JSONMsg(0x17, []byte(`{"task_type":"upgrade","data":{"mcu_type":"A","url":"https://example.invalid/fw","filename":"10-300-11057-37_K10_Manager_V1.71.bin","crc":59102,"size":764904}}`))
	meta := append([]byte{0xa2, 0x35, 0x46, 0x7b, 0x4d, 0x00, 0x04, 0x13, 0x46, 0x69, 0x44, 0x10}, "K10_Master_V1.73"...)
	for p, want := range map[string]string{
		string(task): "firmware update task: 10-300-11057-37_K10_Manager_V1.71.bin (manager)",
		string(meta): "firmware push: K10_Master_V1.73 (267078 B)",
	} {
		got := summarizeFrame(&store.FrameRecord{Class: frame.KindOther, Payload: []byte(p)})
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
