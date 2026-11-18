package api

import (
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/scan"
)

const pathDeviceScan = "/api/v1/device-scan"

// TestDeviceScan (F8): an installer starts a read-only scan; its progress
// is visible; a second start while it runs is a 409; once done, the next
// problem report carries register-scan.jsonl.
func TestDeviceScan(t *testing.T) {
	h := newHarness(t)
	sc := scan.New(scan.GatewayReader(h.gw, "device-scan"), time.Now)
	t.Cleanup(sc.Close)
	h.srv.scanner = sc

	if rr := h.do(t, httpPost, pathDeviceScan, RoleExpert, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("expert: %d", rr.Code)
	}
	rr := h.do(t, httpPost, pathDeviceScan, RoleInstaller, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	if rr := h.do(t, httpPost, pathDeviceScan, RoleInstaller, nil); rr.Code != http.StatusConflict {
		t.Fatalf("second start: %d", rr.Code)
	}
	var st scan.Status
	deadline := time.Now().Add(5 * time.Second)
	for {
		decode(t, h.do(t, httpGet, pathDeviceScan, RoleInstaller, nil), &st)
		if st.State == scan.StateDone || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.State != scan.StateDone || st.Total != 2 || st.OK != 2 {
		t.Fatalf("scan: %+v", st)
	}
	rep := h.do(t, httpPost, pathReport, RoleInstaller, map[string]any{fieldDescr: "with a scan"})
	if _, ok := tarFiles(t, rep.Body.Bytes())["register-scan.jsonl"]; !ok {
		t.Fatal("the report lacks the scan")
	}
}
